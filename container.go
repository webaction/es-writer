package es_writer

import (
	"flag"
	"fmt"
	"os"
	"strconv"
	"sync"
	"time"

	"github.com/streadway/amqp"
	"go.uber.org/zap"
	"gopkg.in/DataDog/dd-trace-go.v1/ddtrace/tracer"

	"gopkg.in/olivere/elastic.v5"
	"gopkg.in/olivere/elastic.v5/config"

	"github.com/go1com/es-writer/action"
)

var (
	retriesInterval = []time.Duration{
		1 * time.Second,
		2 * time.Second,
		3 * time.Second,
		5 * time.Second,
		8 * time.Second,
		13 * time.Second,
		21 * time.Second,
		34 * time.Second,
	}
)

type Container struct {
	Url                  *string
	Kind                 *string
	Exchange             *string
	RoutingKey           *string
	PrefetchCount        *int
	PrefetchSize         *int
	TickInterval         *time.Duration
	QueueName            *string
	UrlContain           *string
	UrlNotContain        *string
	ConsumerName         *string
	EsUrl                *string
	AdminPort            *string
	Debug                *bool
	Refresh              *string
	DataDog              DataDogConfig
	logger               *zap.Logger
	Stop                 chan bool
	SingleActiveConsumer *bool
	BulkTimeoutString    *string
}

type DataDogConfig struct {
	Host        string
	Port        string
	ServiceName string
	Env         string
}

func env(key string, defaultValue string) string {
	value, _ := os.LookupEnv(key)

	if "" == value {
		return defaultValue
	}

	return value
}

func NewContainer(logger *zap.Logger) Container {
	var (
		duration       = env("DURATION", "5")
		iDuration, err = strconv.ParseInt(duration, 10, 64)
	)

	if err != nil {
		logger.Panic("invalid duration", zap.Error(err))
	}

	prefetchCount := env("RABBITMQ_PREFETCH_COUNT", "50")
	iPrefetchCount, err := strconv.Atoi(prefetchCount)
	if err != nil {
		logger.Panic("invalid prefetch-count", zap.Error(err))
	}

	singleActiveConsumer, err := strconv.ParseBool(env("SINGLE_ACTIVE_CONSUMER", "false"))
	if err != nil {
		logger.Panic("invalid single-active-consumer", zap.Error(err))
	}

	debug, err := strconv.ParseBool(env("DEBUG", "false"))
	if err != nil {
		logger.Panic("invalid debug value", zap.Error(err))
	}

	ctn := Container{}
	ctn.Url = flag.String("url", env("RABBITMQ_URL", "amqp://go1:go1@127.0.0.1:5672/"), "")
	ctn.Kind = flag.String("kind", env("RABBITMQ_KIND", "topic"), "")
	ctn.Exchange = flag.String("exchange", env("RABBITMQ_EXCHANGE", "events"), "")
	ctn.RoutingKey = flag.String("routing-key", env("RABBITMQ_ROUTING_KEY", "es.writer.go1"), "")
	ctn.PrefetchCount = flag.Int("prefetch-count", iPrefetchCount, "")
	ctn.PrefetchSize = flag.Int("prefetch-size", 0, "")
	ctn.TickInterval = flag.Duration("tick-iterval", time.Duration(iDuration)*time.Second, "")
	ctn.QueueName = flag.String("queue-name", env("RABBITMQ_QUEUE_NAME", "es-writer"), "")
	ctn.UrlContain = flag.String("url-contains", env("URL_CONTAINS", ""), "")
	ctn.UrlNotContain = flag.String("url-not-contains", env("URL_NOT_CONTAINS", ""), "")
	ctn.ConsumerName = flag.String("consumer-name", env("RABBITMQ_CONSUMER_NAME", "es-writter"), "")
	ctn.EsUrl = flag.String("es-url", env("ELASTIC_SEARCH_URL", "http://127.0.0.1:9200/?sniff=false"), "")
	ctn.Debug = flag.Bool("debug", debug, "Enable with care; credentials can be leaked if this is on.")
	ctn.AdminPort = flag.String("admin-port", env("ADMIN_PORT", ":8001"), "")
	ctn.Refresh = flag.String("refresh", env("ES_REFRESH", "true"), "")
	ctn.SingleActiveConsumer = flag.Bool("single-active-consumer", singleActiveConsumer, "")
	ctn.logger = logger
	bulkTimeout := env("BULK_TIMEOUT", "2m")
	ctn.BulkTimeoutString = flag.String("bulk-timeout", bulkTimeout, "")

	flag.Parse()

	if host := env("DD_AGENT_HOST", ""); host != "" {
		serviceName := flag.String("name", env("SERVICE_NAME", "es-writer"), "")

		ctn.DataDog = DataDogConfig{
			Host:        host,
			Port:        env("DD_AGENT_PORT", "8126"),
			ServiceName: *serviceName,
			Env:         env("DD_ENV", "dev"),
		}

		if ctn.DataDog.Env == "dev" {
			// legacy config
			ctn.DataDog.Env = env("ENVIRONMENT", "dev")
		}
	}

	return ctn
}

func (this *Container) queueConnection() (*amqp.Connection, error) {
	url := *this.Url
	con, err := amqp.Dial(url)

	if nil != err {
		return nil, err
	}

	go func() {
		conCloseChan := con.NotifyClose(make(chan *amqp.Error))

		select {
		case err := <-conCloseChan:
			if err != nil {
				this.logger.Panic("RabbitMQ connection error", zap.Error(err))
			}
		}
	}()

	return con, nil
}

func (this *Container) queueChannel(con *amqp.Connection) (*amqp.Channel, error) {
	ch, err := con.Channel()
	if nil != err {
		return nil, err
	}

	if "topic" != *this.Kind && "direct" != *this.Kind {
		ch.Close()

		return nil, fmt.Errorf("unsupported channel kind: %s", *this.Kind)
	}

	err = ch.ExchangeDeclare(*this.Exchange, *this.Kind, false, false, false, false, nil)
	if nil != err {
		ch.Close()

		return nil, err
	}

	err = ch.Qos(*this.PrefetchCount, *this.PrefetchSize, false)
	if nil != err {
		ch.Close()

		return nil, err
	}

	// Exit when channel closed.
	// @see https://www.rabbitmq.com/channels.html#error-handling
	// @see https://godoc.org/github.com/streadway/amqp#Channel.NotifyClose
	// This will be triggered when the queue is deleted manually on RabbitMQ Management UI.
	go func() {
		chCloseChan := ch.NotifyClose(make(chan *amqp.Error))

		select {
		case err := <-chCloseChan:
			if err != nil {
				this.logger.Error("rabbitMQ channel error", zap.Error(err))
			} else {
				this.logger.Error("rabbitmq channel has been closed")
			}
			this.Stop <- true
		}
	}()

	return ch, nil
}

func (this *Container) elasticSearchClient() (*elastic.Client, error) {
	cfg, err := config.Parse(*this.EsUrl)

	if err != nil {
		this.logger.Panic("failed to parse URL", zap.Error(err))

		return nil, err
	}

	client, err := NewClientFromConfig(cfg, true)
	if err != nil {
		return nil, err
	}

	return client, nil
}

func (this *Container) App() (*App, error, chan bool) {
	con, err := this.queueConnection()
	if err != nil {
		return nil, err, nil
	}

	ch, err := this.queueChannel(con)
	if err != nil {
		return nil, err, nil
	}

	es, err := this.elasticSearchClient()
	if err != nil {
		return nil, err, nil
	}

	this.Stop = make(chan bool)

	go func() {
		<-this.Stop
		ch.Close()
		con.Close()
		os.Exit(1)
	}()

	var bulkTimeOutString string
	var bulkTimeout time.Duration

	if nil != this.BulkTimeoutString {
		bulkTimeOutString = *this.BulkTimeoutString
		bulkTimeout, err = time.ParseDuration(bulkTimeOutString)
		if nil != err {
			return nil, err, nil
		}
	}

	app := &App{
		serviceName: this.DataDog.ServiceName,
		debug:       *this.Debug,
		logger:      this.logger,
		rabbit: &RabbitMqInput{
			ch:     ch,
			tags:   []uint64{},
			logger: this.logger,
		},
		buffer:            action.NewContainer(),
		mutex:             &sync.Mutex{},
		count:             *this.PrefetchCount,
		urlContains:       *this.UrlContain,
		urlNotContains:    *this.UrlNotContain,
		es:                es,
		bulkTimeoutString: bulkTimeOutString,
		bulkTimeout:       bulkTimeout,
		refresh:           *this.Refresh,
		isFlushing:        false,
		isFlushingRWMutex: &sync.RWMutex{},
		spans:             []tracer.Span{},
	}

	app.rabbit.app = app

	return app, nil, this.Stop
}
