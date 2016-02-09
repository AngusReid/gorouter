package main

import (
	"crypto/tls"
	"errors"

	"github.com/apcera/nats"
	cf_debug_server "github.com/cloudfoundry-incubator/cf-debug-server"
	"github.com/cloudfoundry-incubator/cf-lager"
	routing_api "github.com/cloudfoundry-incubator/routing-api"
	token_fetcher "github.com/cloudfoundry-incubator/uaa-token-fetcher"
	"github.com/cloudfoundry/dropsonde"
	"github.com/cloudfoundry/gorouter/access_log"
	vcap "github.com/cloudfoundry/gorouter/common"
	"github.com/cloudfoundry/gorouter/common/secure"
	"github.com/cloudfoundry/gorouter/config"
	"github.com/cloudfoundry/gorouter/proxy"
	rregistry "github.com/cloudfoundry/gorouter/registry"
	"github.com/cloudfoundry/gorouter/route_fetcher"
	"github.com/cloudfoundry/gorouter/router"
	rvarz "github.com/cloudfoundry/gorouter/varz"
	"github.com/cloudfoundry/yagnats"
	"github.com/pivotal-golang/clock"
	"github.com/pivotal-golang/lager"

	"flag"
	"fmt"
	"os"
	"runtime"
	"syscall"
	"time"

	"github.com/cloudfoundry/gorouter/metrics"
	"github.com/tedsuo/ifrit"
	"github.com/tedsuo/ifrit/grouper"
	"github.com/tedsuo/ifrit/sigmon"
)

var configFile string

const (
	DEBUG = "debug"
	INFO  = "info"
	ERROR = "error"
	FATAL = "fatal"
)

func main() {
	flag.StringVar(&configFile, "c", "", "Configuration File")
	cf_lager.AddFlags(flag.CommandLine)
	flag.Parse()

	c := config.DefaultConfig()
	logCounter := vcap.NewLogCounter()

	if configFile != "" {
		c = config.InitConfigFromFile(configFile)
	}

	prefix := "gorouter.stdout"
	if c.Logging.Syslog != "" {
		prefix = c.Logging.Syslog
	}
	logger, _ := cf_lager.New(prefix)

	InitLoggerFromConfig(logger, c, logCounter)
	err := dropsonde.Initialize(c.Logging.MetronAddress, c.Logging.JobName)
	if err != nil {
		logger.Fatal("dropsonde-initialize-error", err)
	}

	// setup number of procs
	if c.GoMaxProcs != 0 {
		runtime.GOMAXPROCS(c.GoMaxProcs)
	}

	if c.DebugAddr != "" {
		cf_debug_server.Run(c.DebugAddr)
	}

	logger.Info("setting-up-nats-connection")
	natsClient := connectToNatsServer(logger.Session("nats"), c)

	metricsReporter := metrics.NewMetricsReporter()
	registry := rregistry.NewRouteRegistry(logger.Session("registry"), c, natsClient, metricsReporter)

	varz := rvarz.NewVarz(registry)
	compositeReporter := metrics.NewCompositeReporter(varz, metricsReporter)

	accessLogger, err := access_log.CreateRunningAccessLogger(logger.Session("access-log"), c)
	if err != nil {
		logger.Fatal("error-creating-access-logger", err)
	}

	var crypto secure.Crypto
	var cryptoPrev secure.Crypto
	if c.RouteServiceEnabled {
		crypto = createCrypto(logger, c.RouteServiceSecret)
		if c.RouteServiceSecretPrev != "" {
			cryptoPrev = createCrypto(logger, c.RouteServiceSecretPrev)
		}
	}

	proxy := buildProxy(logger.Session("proxy"), c, registry, accessLogger, compositeReporter, crypto, cryptoPrev)

	router, err := router.NewRouter(logger.Session("router"), c, proxy, natsClient, registry, varz, logCounter, nil)
	if err != nil {
		logger.Fatal("initialize-router-error", err)
	}

	members := grouper.Members{
		{"router", router},
	}
	if c.RoutingApiEnabled() {
		logger.Info("setting-up-routing-api")
		routeFetcher := setupRouteFetcher(logger.Session("route-fetcher"), c, registry)

		// check connectivity to routing api
		err := routeFetcher.FetchRoutes()
		if err != nil {
			logger.Fatal("routing-api-connection-failed", err)
		}
		members = append(members, grouper.Member{"router-fetcher", routeFetcher})
	}

	group := grouper.NewOrdered(os.Interrupt, members)

	monitor := ifrit.Invoke(sigmon.New(group, syscall.SIGTERM, syscall.SIGINT, syscall.SIGUSR1))

	err = <-monitor.Wait()
	if err != nil {
		logger.Error("gorouter.exited-with-failure", err)
		os.Exit(1)
	}

	os.Exit(0)
}

func createCrypto(logger lager.Logger, secret string) *secure.AesGCM {
	// generate secure encryption key using key derivation function (pbkdf2)
	secretPbkdf2 := secure.NewPbkdf2([]byte(secret), 16)
	crypto, err := secure.NewAesGCM(secretPbkdf2)
	if err != nil {
		logger.Fatal("error-creating-route-service-crypto", err)
	}
	return crypto
}

func buildProxy(logger lager.Logger, c *config.Config, registry rregistry.RegistryInterface, accessLogger access_log.AccessLogger, reporter metrics.ProxyReporter, crypto secure.Crypto, cryptoPrev secure.Crypto) proxy.Proxy {
	args := proxy.ProxyArgs{
		Logger:          logger,
		EndpointTimeout: c.EndpointTimeout,
		Ip:              c.Ip,
		TraceKey:        c.TraceKey,
		Registry:        registry,
		Reporter:        reporter,
		AccessLogger:    accessLogger,
		SecureCookies:   c.SecureCookies,
		TLSConfig: &tls.Config{
			CipherSuites:       c.CipherSuites,
			InsecureSkipVerify: c.SSLSkipValidation,
		},
		RouteServiceEnabled: c.RouteServiceEnabled,
		RouteServiceTimeout: c.RouteServiceTimeout,
		Crypto:              crypto,
		CryptoPrev:          cryptoPrev,
		ExtraHeadersToLog:   c.ExtraHeadersToLog,
	}
	return proxy.NewProxy(args)
}

func setupRouteFetcher(logger lager.Logger, c *config.Config, registry rregistry.RegistryInterface) *route_fetcher.RouteFetcher {
	clock := clock.NewClock()

	tokenFetcher := newTokenFetcher(logger, clock, c)
	_, err := tokenFetcher.FetchToken(false)
	if err != nil {
		logger.Fatal("unable-to-fetch-token", err)
	}

	routingApiUri := fmt.Sprintf("%s:%d", c.RoutingApi.Uri, c.RoutingApi.Port)
	routingApiClient := routing_api.NewClient(routingApiUri)

	routeFetcher := route_fetcher.NewRouteFetcher(logger, tokenFetcher, registry, c, routingApiClient, 1, clock)
	return routeFetcher
}

func newTokenFetcher(logger lager.Logger, clock clock.Clock, c *config.Config) token_fetcher.TokenFetcher {
	if c.RoutingApi.AuthDisabled {
		logger.Info("using-noop-token-fetcher")
		return token_fetcher.NewNoOpTokenFetcher()
	}
	tokenFetcherConfig := token_fetcher.TokenFetcherConfig{
		MaxNumberOfRetries:   c.TokenFetcherMaxRetries,
		RetryInterval:        c.TokenFetcherRetryInterval,
		ExpirationBufferTime: c.TokenFetcherExpirationBufferTimeInSeconds,
	}

	logger.Info("fetching-token-from-uaa")
	tokenFetcher, err := token_fetcher.NewTokenFetcher(logger, &c.OAuth, tokenFetcherConfig, clock)
	if err != nil {
		logger.Fatal("initialize-token-fetcher-error", err)
	}
	return tokenFetcher
}

func connectToNatsServer(logger lager.Logger, c *config.Config) yagnats.NATSConn {
	var natsClient yagnats.NATSConn
	var err error

	natsServers := c.NatsServers()
	attempts := 3
	for attempts > 0 {
		natsClient, err = yagnats.Connect(natsServers)
		if err == nil {
			break
		} else {
			attempts--
			time.Sleep(100 * time.Millisecond)
		}
	}

	if err != nil {
		logger.Fatal("nats-connection-error", err)
	}

	natsClient.AddClosedCB(func(conn *nats.Conn) {
		logger.Fatal("nats-connection-closed", errors.New("unexpected close"), lager.Data{"connection": *conn})
	})

	return natsClient
}

func InitLoggerFromConfig(logger lager.Logger, c *config.Config, logCounter *vcap.LogCounter) {
	if c.Logging.File != "" {
		file, err := os.OpenFile(c.Logging.File, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0666)
		if err != nil {
			logger.Fatal("error-opening-log-file", err, lager.Data{"file": c.Logging.File})
		}
		var logLevel lager.LogLevel
		switch c.Logging.Level {
		case DEBUG:
			logLevel = lager.DEBUG
		case INFO:
			logLevel = lager.INFO
		case ERROR:
			logLevel = lager.ERROR
		case FATAL:
			logLevel = lager.FATAL
		default:
			panic(fmt.Errorf("unknown log level: %s", c.Logging.Level))
		}
		logger.RegisterSink(lager.NewWriterSink(file, logLevel))
	}

	logger.RegisterSink(logCounter)
}
