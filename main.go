package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"github.com/lib/pq"
	log "github.com/sirupsen/logrus"
	"gopkg.in/tucnak/telebot.v2"
	"gorm.io/driver/mysql"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"io"
	"io/ioutil"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

type Config struct {
	Telegram struct {
		DebugEndpoint string `json:"debug_endpoint"`
		BotToken      string `json:"bot_token"`
	} `json:"telegram"`

	Database struct {
		Kind string `json:"kind"` // Could be "sqlite" or "mysql"
		DSN  string `json:"dsn"`
	} `json:"database"`

	Log struct {
		File  string `json:"file"`
		Level uint32 `json:"level"`
	} `json:"log"`

	Network struct {
		Proxy string `json:"proxy"`
	} `json:"network"`

	XMR struct {
		CryptoCompareAPIKey string `json:"crypto_compare_api_key"`
		FetchDuration       int    `json:"fetch_duration"` // default: 10s
	} `json:"xmr"`
}

type Bot struct {
	xmrPriceMu   sync.RWMutex
	currentPrice XMRPrice

	config Config
	logger *log.Logger

	tb  *telebot.Bot
	xmr *XMRPriceFetcher

	db *gorm.DB

	notifiersMu sync.RWMutex
	notifiers   map[int64]*Notifier

	sendMsgMu sync.Mutex
}

type Alerts struct {
	BTC pq.Float64Array `gorm:"column:btc;type:float[]"`
	USD pq.Float64Array `gorm:"column:usd;type:float[]"`
	EUR pq.Float64Array `gorm:"column:eur;type:float[]"`
	CNY pq.Float64Array `gorm:"column:cny;type:float[]"`
}

type Notifier struct {
	bot       *Bot
	lastPrice XMRPrice

	ChatId int64  `gorm:"primaryKey"`
	Alerts Alerts `gorm:"embedded"`
}

type alertKind int

func alertKindFromString(str string) (alertKind, error) {
	str = strings.ToLower(str)
	switch str {
	case "btc":
		return BTC, nil
	case "usd":
		return USD, nil
	case "eur":
		return EUR, nil
	case "cny":
		return CNY, nil
	}

	return -1, fmt.Errorf("invalid alert kind")
}

func (a alertKind) String() string {
	switch a {
	case BTC:
		return "btc"
	case USD:
		return "usd"
	case EUR:
		return "eur"
	case CNY:
		return "cny"
	default:
		panic("unsupported currency type")
	}
}

const (
	BTC alertKind = iota
	USD
	EUR
	CNY
)

func (b *Bot) newNotifier(chatId int64) *Notifier {
	// recheck if the requested notifier has already been created
	if _, ok := b.notifiers[chatId]; ok {
		return b.notifiers[chatId]
	}

	n := &Notifier{
		lastPrice: b.currentPrice,
		bot:       b,
		ChatId:    chatId,
	}

	b.notifiers[chatId] = n

	b.db.Create(n)

	return n
}

func loadAllNotifiersFromDatabase(initialPrice XMRPrice, bot *Bot) (map[int64]*Notifier, error) {
	notifiers := make([]Notifier, 0)
	notifierMaps := make(map[int64]*Notifier)
	err := bot.db.Find(&notifiers).Error
	for _, notifier := range notifiers {
		notifier.lastPrice = initialPrice
		notifier.bot = bot
		notifierMaps[notifier.ChatId] = &notifier
	}

	return notifierMaps, err
}

func (n *Notifier) addAlert(kind alertKind, price float64) error {
	switch kind {
	case BTC:
		n.Alerts.BTC = append(n.Alerts.BTC, price)
		sort.Float64s(n.Alerts.BTC)
	case USD:
		n.Alerts.USD = append(n.Alerts.USD, price)
		sort.Float64s(n.Alerts.USD)
	case EUR:
		n.Alerts.EUR = append(n.Alerts.EUR, price)
		sort.Float64s(n.Alerts.EUR)
	case CNY:
		n.Alerts.CNY = append(n.Alerts.CNY, price)
		sort.Float64s(n.Alerts.CNY)
	default:
		panic("invalid alert kind")
	}

	return n.bot.db.Save(n).Error
}

func removeElementAtIndex(slice []float64, index int) ([]float64, error) {
	if index < 0 || index > len(slice) {
		return nil, fmt.Errorf("index %v out of range", index)
	}

	return append(slice[:index], slice[index+1:]...), nil
}

func (n *Notifier) removeAlert(kind alertKind, index int) error {
	switch kind {
	case BTC:
		if arr, err := removeElementAtIndex(n.Alerts.BTC, index); err != nil {
			return err
		} else {
			n.Alerts.BTC = arr
		}
	case USD:
		if arr, err := removeElementAtIndex(n.Alerts.USD, index); err != nil {
			return err
		} else {
			n.Alerts.USD = arr
		}
	case EUR:
		if arr, err := removeElementAtIndex(n.Alerts.EUR, index); err != nil {
			return err
		} else {
			n.Alerts.EUR = arr
		}
	case CNY:
		if arr, err := removeElementAtIndex(n.Alerts.CNY, index); err != nil {
			return err
		} else {
			n.Alerts.CNY = arr
		}
	default:
		panic("invalid alert kind")
	}

	return n.bot.db.Save(n).Error
}

func (n *Notifier) removeAllAlerts() error {
	n.Alerts.USD = nil
	n.Alerts.BTC = nil
	n.Alerts.EUR = nil
	n.Alerts.CNY = nil

	return n.bot.db.Save(n).Error
}

func (n *Notifier) alert(kind alertKind, price float64, lastPrice float64) {
	priceStr := fmt.Sprintf("%v %s", price, kind)
	alertMessage := "Price alert: xmr price has "
	if price > lastPrice {
		alertMessage += "raised above"
	} else {
		alertMessage += "fallen below"
	}
	alertMessage += " "
	alertMessage += priceStr
	n.bot.sendStringMessage(telebot.ChatID(n.ChatId), alertMessage)
}

func (n *Notifier) updatePrice(price XMRPrice) {
	var lowerPrice, higherPrice XMRPrice

	// last price < current price
	if n.lastPrice.Less(price) {
		lowerPrice, higherPrice = n.lastPrice, price
	} else {
		lowerPrice, higherPrice = price, n.lastPrice
	}

	for _, price := range n.Alerts.BTC {
		if !lowerPrice.compareWithKind(BTC, price) &&
			higherPrice.compareWithKind(BTC, price) {
			n.alert(BTC, price, n.lastPrice.BTC)
		}
	}

	for _, price := range n.Alerts.USD {
		if !lowerPrice.compareWithKind(USD, price) &&
			higherPrice.compareWithKind(USD, price) {
			n.alert(USD, price, n.lastPrice.USD)
		}
	}

	for _, price := range n.Alerts.EUR {
		if !lowerPrice.compareWithKind(EUR, price) &&
			higherPrice.compareWithKind(EUR, price) {
			n.alert(EUR, price, n.lastPrice.EUR)
		}
	}

	for _, price := range n.Alerts.CNY {
		if !lowerPrice.compareWithKind(CNY, price) &&
			higherPrice.compareWithKind(CNY, price) {
			n.alert(CNY, price, n.lastPrice.CNY)
		}
	}

	n.lastPrice = price
}

// Implement the following bot commands
// - /xmralert help ???
// - /xmralert list ???
// - /xmralert add <currency(btc|usd|eur|cny)> <price(float64)> ???
// - /xmralert remove <currency(btc|usd|eur|cny)> <index(int)> ???
// - /xmrprice ???

const AlertCommand = "/xmralert"
const PriceCommand = "/xmrprice"

const MysqlDatabaseKind = "mysql"
const SqliteDatabaseKind = "sqlite"

const MysqlDatabaseDSNEnvKey = "MYSQL_DATABASE_DSN"

const TryLimit = 5

func NewBot(config Config) (bot *Bot, err error) {
	if config.Database.DSN == "" {
		if envVar := os.Getenv(MysqlDatabaseDSNEnvKey); envVar != "" {
			config.Database.Kind = "mysql"
			config.Database.DSN = envVar
			log.Warningf("using mysql database config from environment: %v", envVar)
		}
	}

	bot = &Bot{
		config: config,
		logger: log.New(),
	}

	// Setup logger

	if config.Log.File != "" {
		logFile, err := os.OpenFile(config.Log.File, os.O_WRONLY|os.O_APPEND|os.O_CREATE|os.O_TRUNC, 0666)

		if err != nil {
			bot.logger.Warnf("failed to open log file: %v", err)
		} else {
			writer := io.MultiWriter(logFile, os.Stdout)
			bot.logger.SetOutput(writer)
		}
	}

	bot.logger.SetLevel(log.Level(config.Log.Level))

	// Set up xmr
	if bot.xmr, err = NewXMRPriceFetcher(config); err != nil {
		return nil, err
	}

	price, err := bot.xmr.FetchPrice(config.XMR.CryptoCompareAPIKey)
	if err != nil {
		bot.logger.Errorf("failed to fetch xmr price: %v", err)
		return nil, err
	}
	bot.currentPrice = price

	switch config.Database.Kind {
	case SqliteDatabaseKind:
		bot.db, err = gorm.Open(sqlite.Open(config.Database.DSN), &gorm.Config{})
	case MysqlDatabaseKind:
		for i := 0; i < TryLimit; i++ {
			bot.db, err = gorm.Open(mysql.Open(config.Database.DSN), &gorm.Config{})
			if err == nil {
				break
			}
			time.Sleep(3 * time.Second)
		}
	}

	if err != nil {
		bot.logger.Errorf("failed to connect to database: %v", err)
		return nil, err
	}

	if err := bot.db.AutoMigrate(&Notifier{}); err != nil {
		bot.logger.Errorf("failed to migrate database: %v", err)
		return nil, err
	}

	bot.notifiers, err = loadAllNotifiersFromDatabase(bot.currentPrice, bot)
	if err != nil {
		bot.logger.Errorf("failed to load notifiers: %v", err)
		return nil, err
	}

	// Set up telegram bot
	var botClient *http.Client

	if config.Network.Proxy != "" {
		proxyUrl, err := url.Parse(config.Network.Proxy)
		if err != nil {
			bot.logger.Errorf("failed to set up proxy: %v", err)
			return nil, err
		}

		botClient = &http.Client{
			Transport: &http.Transport{
				Proxy: http.ProxyURL(proxyUrl),
			},
		}
	}

	if bot.tb, err = telebot.NewBot(telebot.Settings{
		URL:   config.Telegram.DebugEndpoint,
		Token: config.Telegram.BotToken,
		Poller: &telebot.LongPoller{
			Limit:   10,
			Timeout: 6 * time.Second,
			AllowedUpdates: []string{
				"message",
			},
		},
		Client: botClient,
	}); err != nil {
		return nil, err
	}

	bot.tb.Handle(AlertCommand, bot.handleAlertCommand)
	bot.tb.Handle(PriceCommand, bot.handlePriceCommand)

	bot.xmr.Subscribe(bot.handleXMRPrice)

	return
}

func (b *Bot) sendStringMessage(to telebot.Recipient, msg string) {
	go func() {
		b.sendMsgMu.Lock()
		defer b.sendMsgMu.Unlock()

		for i := 0; i < TryLimit; i++ {
			if _, err := b.tb.Send(to, msg); err != nil {
				b.logger.Warnf("failed to send message: %v", err)
			} else {
				return
			}
		}

		b.logger.Warnf("unable to deliver message %v to %v", msg, to)
	}()
}

//goland:noinspection GrazieInspection
const alertHelpMessage = `
Usage: /xmralert <Subcommand>

Subcommands:
    - help
        Show this help message.
    - list
        List all alerts. 
        Each row of the response message presents an alert, which is organized in the following format:
          <index> <price>
        The index can be used to remove an alert.
    - add <currency> <price>
        Add a alert
        - currency 
          Could be one of btc,usd,eur end cny
        - price
          A number
    - remove <currency> <index>
        Remove an alert. 
    - removeAll
        Remove all the alerts.
`

func (b *Bot) handleAlertCommand(m *telebot.Message) {
	cmdComponents := strings.Split(m.Text, " ")
	var subCommand string

	subCommands := map[string]func(chatId int64, parameters []string) (string, error){
		"add":       b.addAlert,
		"remove":    b.removeAlert,
		"removeAll": b.removeAllAlert,
		"list":      b.listAlerts,
	}

	if len(cmdComponents) < 2 {
		subCommand = "help"
	} else {
		subCommand = cmdComponents[1]
		if _, ok := subCommands[subCommand]; !ok {
			subCommand = "help"
		}
	}

	var message string
	var err error
	switch subCommand {
	case "help":
		message = alertHelpMessage
	default:
		message, err = subCommands[subCommand](m.Chat.ID, cmdComponents[2:])
		if err != nil {
			b.logger.Warnf("subcommand failed: %v", err)
			message = fmt.Sprintf("Error: %v\n\n%s", err, alertHelpMessage)
		}
	}

	b.sendStringMessage(m.Chat, message)
}

func (b *Bot) listAlerts(chatId int64, parameters []string) (string, error) {
	if len(parameters) != 0 {
		return "", invalidParametersError
	}

	var message string

	notifier := b.getNotifier(chatId)

	message += "BTC:\n"

	for index, price := range notifier.Alerts.BTC {
		message += fmt.Sprintf("    %v: %v\n", index, price)
	}

	message += "USD:\n"

	for index, price := range notifier.Alerts.USD {
		message += fmt.Sprintf("    %v: %v\n", index, price)
	}

	message += "EUR:\n"

	for index, price := range notifier.Alerts.EUR {
		message += fmt.Sprintf("    %v: %v\n", index, price)
	}

	message += "CNY:\n"

	for index, price := range notifier.Alerts.CNY {
		message += fmt.Sprintf("    %v: %v\n", index, price)
	}

	return message, nil
}

var invalidParametersError = fmt.Errorf("invalid parameters")

func (b *Bot) addAlert(chatId int64, parameters []string) (string, error) {
	if len(parameters) != 2 {
		return "", invalidParametersError
	}

	currency, err := alertKindFromString(parameters[0])
	if err != nil {
		return "", invalidParametersError
	}

	price, err := strconv.ParseFloat(parameters[1], 64)
	if err != nil {
		return "", invalidParametersError
	}

	n := b.getNotifier(chatId)
	if err := n.addAlert(currency, price); err != nil {
		return "", err
	}

	return fmt.Sprintf("Alert added: (%s) %v", currency, price), nil
}

func (b *Bot) getNotifier(chatId int64) *Notifier {
	b.notifiersMu.RLock()
	if notifier, ok := b.notifiers[chatId]; ok {
		b.notifiersMu.RUnlock()
		return notifier
	} else {
		b.notifiersMu.RUnlock()
		b.notifiersMu.Lock()
		b.xmrPriceMu.RLock()
		n := b.newNotifier(chatId)
		b.xmrPriceMu.RUnlock()
		b.notifiersMu.Unlock()
		return n
	}
}

func (b *Bot) removeAlert(chatId int64, parameters []string) (string, error) {
	if len(parameters) != 2 {
		return "", invalidParametersError
	}

	currency, err := alertKindFromString(parameters[0])
	if err != nil {
		return "", invalidParametersError
	}

	index, err := strconv.ParseInt(parameters[1], 10, 32)
	if err != nil {
		return "", invalidParametersError
	}

	n := b.getNotifier(chatId)
	if err := n.removeAlert(currency, int(index)); err != nil {
		return "", err
	}

	return "Alert removed", nil
}

func (b *Bot) removeAllAlert(chatId int64, parameters []string) (string, error) {
	if len(parameters) != 0 {
		return "", invalidParametersError
	}

	n := b.getNotifier(chatId)
	if err := n.removeAllAlerts(); err != nil {
		return "", err
	}

	return "All alert removed", nil
}

func (b *Bot) handlePriceCommand(m *telebot.Message) {
	b.xmrPriceMu.RLock()
	price := b.currentPrice
	b.xmrPriceMu.RUnlock()

	b.sendStringMessage(m.Chat, price.String())
}

func (b *Bot) handleXMRPrice(price XMRPrice) {
	b.xmrPriceMu.Lock()
	b.currentPrice = price
	b.xmrPriceMu.Unlock()

	b.notifiersMu.RLock()
	defer b.notifiersMu.RUnlock()

	for _, notifier := range b.notifiers {
		notifier.updatePrice(price)
	}
}

func (b *Bot) Stop() {
	b.tb.Stop()
	b.xmr.Stop()
}

func (b *Bot) Run() {
	b.tb.Start()

	signalChannel := make(chan os.Signal)
	signal.Notify(signalChannel, os.Interrupt, os.Kill)

	<-signalChannel
	b.Stop()
}

const XMRPriceAPIEndpoint = "https://min-api.cryptocompare.com/data/price?fsym=XMR&tsyms=BTC,USD,EUR,CNY&apiKey=%s"

type XMRPrice struct {
	BTC float64 `json:"BTC"`
	USD float64 `json:"USD"`
	EUR float64 `json:"EUR"`
	CNY float64 `json:"CNY"`
}

// Less determine if x is smaller than p
func (x XMRPrice) Less(p XMRPrice) bool {
	return x.BTC < p.BTC
}

func (x XMRPrice) compareWithKind(kind alertKind, val float64) bool {
	switch kind {
	case BTC:
		return x.BTC < val
	case USD:
		return x.USD < val
	case EUR:
		return x.EUR < val
	case CNY:
		return x.CNY < val
	}

	panic("invalid alert kind")
}

func (x XMRPrice) String() string {
	return fmt.Sprintf(`
Current XMR Price
    BTC: %v
    USD: %v
    EUR: %v
    CNY: %v
`, x.BTC, x.USD, x.EUR, x.CNY)
}

// XMRPriceFetcher fetches xmr price every several seconds
type XMRPriceFetcher struct {
	cachedPrice XMRPrice
	client      *http.Client
	logger      *log.Logger
	cancelFunc  context.CancelFunc

	subFuncs []func(price XMRPrice)
}

func (x *XMRPriceFetcher) Subscribe(subFunc func(XMRPrice)) {
	x.subFuncs = append(x.subFuncs, subFunc)
}

func (x *XMRPriceFetcher) worker(duration int, apiKey string, ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.NewTimer(time.Second * time.Duration(duration)).C:
			x.fetchPrice(apiKey)
		}
	}
}

func (x *XMRPriceFetcher) fetchPrice(apiKey string) {
	newPrice, err := x.FetchPrice(apiKey)
	if err != nil {
		return
	}

	for _, subFunc := range x.subFuncs {
		subFunc(newPrice)
	}
}

func (x *XMRPriceFetcher) FetchPrice(apiKey string) (XMRPrice, error) {
	resp, err := x.client.Get(fmt.Sprintf(XMRPriceAPIEndpoint, apiKey))

	if err != nil {
		x.logger.Warnf("failed to fetch xmr price: %v", err)
		return XMRPrice{}, err
	}

	//goland:noinspection GoUnhandledErrorResult
	defer resp.Body.Close()

	var price XMRPrice

	if err := json.NewDecoder(resp.Body).Decode(&price); err != nil {
		x.logger.Warnf("failed to decode xmr price: %v", err)
		return XMRPrice{}, err
	}

	return price, nil
}

func (x *XMRPriceFetcher) Stop() {
	x.cancelFunc()
}

func NewXMRPriceFetcher(config Config) (*XMRPriceFetcher, error) {
	client := new(http.Client)

	if config.Network.Proxy != "" {
		proxyUrl, err := url.Parse(config.Network.Proxy)
		if err != nil {
			return nil, err
		}

		client.Transport = &http.Transport{
			Proxy: http.ProxyURL(proxyUrl),
		}
	}

	fetcher := &XMRPriceFetcher{
		client: client,
		logger: log.New(),
	}

	duration := config.XMR.FetchDuration
	if duration <= 0 {
		duration = 10 // fetch price every 10s by default
	}

	fetcher.fetchPrice(config.XMR.CryptoCompareAPIKey) // fetch initial price

	// Bootstrap worker

	workerCtx, cancelFunc := context.WithCancel(context.Background())
	fetcher.cancelFunc = cancelFunc

	go fetcher.worker(duration, config.XMR.CryptoCompareAPIKey, workerCtx)

	return fetcher, nil
}

var config = flag.String("config", "config.json", "path to config file")

func main() {
	flag.Parse()

	data, err := ioutil.ReadFile(*config)

	if err != nil {
		log.Fatalf("failed to read configuration file: %v", err)
	}

	var config Config

	if err := json.Unmarshal(data, &config); err != nil {
		log.Fatalf("failed to decode configuration file: %v", err)
	}

	bot, err := NewBot(config)

	if err != nil {
		log.Fatalf("failed to set up bot: %v", err)
	}

	bot.Run()
}
