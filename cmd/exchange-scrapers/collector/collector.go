package main

import (
	"flag"
	"sync"
	"time"

	"github.com/diadata-org/diadata/pkg/dia/helpers/configCollectors"
	scrapers "github.com/diadata-org/diadata/pkg/dia/scraper/exchange-scrapers"
	"github.com/diadata-org/diadata/pkg/utils"
	"github.com/diadata-org/diadata/pkg/utils/probes"

	"github.com/diadata-org/diadata/pkg/dia"
	"github.com/diadata-org/diadata/pkg/dia/helpers/kafkaHelper"
	models "github.com/diadata-org/diadata/pkg/model"
	"github.com/segmentio/kafka-go"
	"github.com/sirupsen/logrus"
)

var (
	log                  *logrus.Logger
	swapTradesOnExchange = []string{
		dia.AnyswapExchange,
		dia.CurveFIExchange,
		dia.CurveFIExchangeFantom,
		dia.CurveFIExchangeMoonbeam,
		dia.CurveFIExchangePolygon,
		dia.CurveFIExchangeArbitrum,
		dia.DiffusionExchange,
		dia.HermesExchange,
		dia.HuckleberryExchange,
		dia.MaverickExchange,
		dia.NetswapExchange,
		dia.OmniDexExchange,
		dia.TraderJoeExchangeV2_1Avalanche,
		dia.WanswapExchange,
		dia.VelodromeExchange,
		dia.VelodromeSlipstreamExchange,
		dia.ZenlinkswapExchange,
		dia.ZenlinkswapExchangeBifrostPolkadot,
		dia.PanCakeSwapExchangeV3,
		dia.VelarExchange,
		dia.BifrostExchange,
		dia.HydrationExchange,
		dia.UniswapExchangeV3Celo,
		dia.UniswapExchangeV4,
		dia.VelodromeExchangeSwellchain,
		dia.ShadowV2Exchange,
		dia.ShadowV3Exchange,
	}

	exchange = flag.String("exchange", "", "which exchange")
	// mode==current:		default mode. Trades are forwarded to TBS and FBS.
	// mode==storeTrades:	trades are not forwarded to TBS and FBS and stored as raw trades in influx.
	// mode==estimation:	trades are forwarded to tradesEstimationService, i.e. same as storeTrades mode
	//						but estimatedUSDPrice is filled by tradesEstimationService.
	// mode==historical:	trades are sent through kafka to TBS in tradesHistorical topic.
	// mode==assetmap:   	Bridged Trades, asstes are mapped and trades are not saved.
	mode              = flag.String("mode", "current", "either storeTrades, current, historical or estimation.")
	pairsfile         = flag.Bool("pairsfile", false, "read pairs from json file in config folder.")
	replicaKafkaTopic string

	startupDone bool
)

func init() {
	log = logrus.New()
	flag.Parse()
	if *exchange == "" {
		flag.Usage()
		for e := range scrapers.Exchanges {
			log.Info("exchange: ", e)
		}
		for {
			time.Sleep(24 * time.Hour)
		}
	}
	if !isValidExchange(*exchange) {
		log.Fatal("Invalid exchange string: ", *exchange)
	}
	replicaKafkaTopic = utils.Getenv("REPLICA_KAFKA_TOPIC", "false")
}

func ready() bool {
	return startupDone
}

func live() bool {
	return true
}

// main manages all PairScrapers and handles incoming trade information
func main() {

	log.Infof("start collector for %s in %s mode...", *exchange, *mode)
	log.Infoln("starting probes")
	probes.Start(live, ready)

	relDB, err := models.NewRelDataStore()
	if err != nil {
		log.Errorln("NewDataStore:", err)
	}

	ds, err := models.NewDataStore()
	if err != nil {
		log.Fatal("datastore: ", err)
	}

	// Fetch exchange pairs from database or json file in config folder.
	var pairsExchange []dia.ExchangePair
	if !*pairsfile {
		pairsExchange, err = relDB.GetExchangePairSymbols(*exchange)
		if err != nil {
			log.Fatal("fetch pairs from database: ", err)
		}
	} else {
		log.Error("error on GetExchangePairSymbols", err)
		cc := configCollectors.NewConfigCollectors(*exchange, ".json")
		pairsExchange = cc.AllPairs()
	}
	log.Info("available exchangePairs:", len(pairsExchange))

	// Make API scraper.
	configApi, err := dia.GetConfig(*exchange)
	if err != nil {
		log.Warning("no config for exchange's api ", err)
	}
	es := scrapers.NewAPIScraper(*exchange, true, configApi.ApiKey, configApi.SecretKey, relDB)

	// Set up kafka writers for various modes.
	var (
		w *kafka.Writer
		// This topic can be used to forward trades to services other than the prod. tradesblockservice.
		wReplica *kafka.Writer
	)

	switch *mode {
	case "current":
		w = kafkaHelper.NewWriter(kafkaHelper.TopicTrades)
		wReplica = kafkaHelper.NewWriter(kafkaHelper.TopicTradesReplica)
	case "estimation":
		w = kafkaHelper.NewWriter(kafkaHelper.TopicTradesEstimation)
	case "assetmap":
		w = kafkaHelper.NewWriter(kafkaHelper.TopicTradesEstimation)
	}

	if *mode != "storeTrades" {
		defer func() {
			err := w.Close()
			if err != nil {
				log.Error(err)
			}
		}()
	}

	wg := sync.WaitGroup{}

	if scrapers.Exchanges[*exchange].Centralized || scrapers.ExchangeDuplicates[*exchange].Centralized {

		// Scrape pairs for CEX scrapers.
		for _, configPair := range pairsExchange {
			log.Println("Adding pair:", configPair.Symbol, configPair.ForeignName, "on exchange", *exchange)
			_, err := es.ScrapePair(dia.ExchangePair{
				Symbol:      configPair.Symbol,
				ForeignName: configPair.ForeignName})
			if err != nil {
				log.Println(err)
			} else {
				wg.Add(1)
			}
			defer wg.Wait()
		}

	} else {

		// Subscription to pool events managed inside scraper for DEX and Bridge scrapers.
		wg.Add(1)
		defer wg.Wait()

	}
	startupDone = true
	go handleTrades(es.Channel(), &wg, w, wReplica, ds, *exchange, *mode)
}

func handleTrades(c chan *dia.Trade, wg *sync.WaitGroup, w *kafka.Writer, wReplica *kafka.Writer, ds *models.DB, exchange string, mode string) {
	lastTradeTime := time.Now()
	watchdogDelay := scrapers.Exchanges[exchange].WatchdogDelay
	if watchdogDelay == 0 {
		watchdogDelay = scrapers.ExchangeDuplicates[exchange].WatchdogDelay
	}
	t := time.NewTicker(time.Duration(watchdogDelay) * time.Second)
	for {
		select {
		case <-t.C:
			duration := time.Since(lastTradeTime)
			if duration > time.Duration(watchdogDelay)*time.Second {
				log.Error(duration)
				panic("frozen? ")
			}
		case t, ok := <-c:
			if !ok {
				wg.Done()
				log.Error("handleTrades")
				return
			}
			lastTradeTime = t.Time
			// Trades are sent to the tradesblockservice through a kafka channel - either
			// through trades topic or historical trades topic.
			if mode == "current" || mode == "historical" || mode == "estimation" {

				// Write trade to productive Kafka.
				err := writeTradeToKafka(w, t)
				if err != nil {
					log.Error(err)
				}

				if replicaKafkaTopic == "true" {
					err := writeTradeToKafka(wReplica, t)
					if err != nil {
						log.Error(err)
					}
				}

			}
			// Trades are just saved in influx - not sent to the tradesblockservice through a kafka channel.
			if mode == "storeTrades" {
				err := ds.SaveTradeInflux(t)
				if err != nil {
					log.Error(err)
				}
				err = ds.Flush()
				if err != nil {
					log.Error("Flush Influx in storeTrades mode: ", err)
				}
			}

			if mode == "assetmap" {
				log.Info("recieved trade", t)
			}
		}
	}
}

func writeTradeToKafka(w *kafka.Writer, t *dia.Trade) error {
	// Write trade to Kafka.
	err := kafkaHelper.WriteMessage(w, t)
	if err != nil {
		return err
	}

	// Write reversed trade to Kafka as well for some exchanges.
	if utils.Contains(&swapTradesOnExchange, t.Source) {
		tSwapped, err := dia.SwapTrade(*t)
		if err != nil {
			log.Error("swap trade: ", err)
		} else {
			err = kafkaHelper.WriteMessage(w, &tSwapped)
			if err != nil {
				return err
			}
		}
	}
	return nil
}

func isValidExchange(estring string) bool {
	for e := range scrapers.Exchanges {
		if e == estring {
			return true
		}
	}
	for e := range scrapers.ExchangeDuplicates {
		if e == estring {
			return true
		}
	}
	return false
}
