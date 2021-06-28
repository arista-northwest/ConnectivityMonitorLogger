// Copyright (c) 2019 Arista Networks, Inc.  All rights reserved.
// Arista Networks, Inc. Confidential and Proprietary.

package main

import (
	"context"
	"errors"
	"flag"

	"path"
	"reflect"
	"strings"
	"time"

	"log"

	"github.com/arista-northwest/ConnectivityMonitorLogger/logger"
	"github.com/aristanetworks/goarista/gnmi"
	pb "github.com/openconfig/gnmi/proto/gnmi"
)

var (
	cfg         = &gnmi.Config{TLS: true}
	subOpts     = &gnmi.SubscribeOptions{}
	query       = "/Sysdb/connectivityMonitor/status/hostStatus"
	eventLogger interface{}

	rttBaselines   = make(map[string]float32)
	monitorMetrics = make(map[string]*monitorMetric)
	triggerStates  = make(map[string]*triggerState)

	eosNative = flag.Bool("eos_native", false, "set to true if 'eos_native' is enabled under 'management api gnmi'")
	interval  = flag.Uint64("interval", 5, "Set to interval configured under 'monitor connectivity (default is 5")

	triggerCount = flag.Uint("trigger_count", 3, "Number of events to trigger an alert (default is 3)")
	rearmCount   = flag.Uint("rearm_count", 2, "Number of events to rearm a previously triggered alert (default is 2)")

	rttBaselineTrigger    = flag.Uint("rtt_baseline_trigger", 10, "Sets a new baseline after N deviations from old baseline")
	rttDeviationThreshold = flag.Uint64("rtt_deviation_threshold", 10, "Percent RTT deviation above or below baseline")
	rttThreshold          = flag.Float64("rtt_threshold", 0, "Absolute absolute value for RTT")
	lossThreshold         = flag.Uint64("loss_threshold", 0.0, "Perect of lost packets to tolerate")

	// Certificate files.
	insecure = flag.Bool("insecure", false, "use insecure GRPC connection.")
	// caCert     = flag.String("ca_crt", "", "CA certificate file. Used to verify server TLS certificate.")
	// clientCert = flag.String("client_crt", "", "Client certificate file. Used for client certificate-based authentication.")
	// clientKey  = flag.String("client_key", "", "Client private key file. Used for client certificate-based authentication.")
	// //insecureSkipVerify = flag.Bool("tls_skip_verify", false, "When set, CLI will not verify the server certificate during TLS handshake.")

	logInit        = logger.LogD("CONNECTIVITYMONITOR_INIT", logger.INFO, "Agent Initialized", "Agent has been initialized", logger.NoActionRequired, time.Duration(time.Second*10), 10)
	logRttTrigger  = logger.LogD("CONNECTIVITYMONITOR_RTT_TRIGGER", logger.NOTICE, "RTT triggered event on %s rtt:%.2fms threshold:%.2fms", "", logger.NoActionRequired, time.Duration(time.Second), 10)
	logRttRearm    = logger.LogD("CONNECTIVITYMONITOR_RTT_REARM", logger.NOTICE, "RTT rearmed event on %s rtt:%.2fms", "", logger.NoActionRequired, time.Duration(time.Second*10), 10)
	logLossTrigger = logger.LogD("CONNECTIVITYMONITOR_LOSS_TRIGGER", logger.NOTICE, "Packet loss triggered event on %s loss:%d%% threshold:%d%%", "", logger.NoActionRequired, time.Duration(time.Second*10), 10)
	loglossRearm   = logger.LogD("CONNECTIVITYMONITOR_LOSS_REARM", logger.NOTICE, "Packet loss rearmed event on %s loss:%d%%", "", logger.NoActionRequired, time.Duration(time.Second*10), 10)
	logRttBaseline = logger.LogD("CONNECTIVITYMONITOR_RTT_BASELINE", logger.NOTICE, "Setting new baseline on %s to %.2f was %.2f", "", logger.NoActionRequired, time.Duration(time.Second*10), 10)
)

type monitorMetric struct {
	Timestamp        string  `alias:"timestamp"`
	HostName         string  `alias:"hostName"`
	VRF              string  `alias:"vrfName"`
	IpAddr           string  `alias:"ipAddr"`
	Interface        string  `alias:"interfaceName"`
	Latency          float32 `alias:"latency"`
	Jitter           float32 `alias:"jitter"`
	PacketLoss       uint64  `alias:"packetLoss"`
	HTTPResponseTime float32 `alias:"httpResponseTime"`
}

const (
	Armed = iota
	Triggered
)

type triggerState struct {
	current int32
	offset  uint
}

func (t *triggerState) trigger(max uint) bool {
	if t.current == Triggered {
		return false
	}
	t.offset += 1
	if t.offset >= max {
		t.current = Triggered
		t.offset = 0
		return true
	}

	return false
}

func (t *triggerState) rearm(max uint) bool {
	if t.current == Armed {
		return false
	}
	t.offset += 1
	if t.offset >= max {
		t.current = Armed
		t.offset = 0
		return true
	}

	return false
}

func init() {
	flag.StringVar(&cfg.Addr, "address", "localhost:6030", "Address of the GNMI target to query.")
	flag.StringVar(&cfg.Username, "username", "admin", "")
	flag.StringVar(&cfg.Password, "password", "", "")
	flag.StringVar(&cfg.CAFile, "cafile", "", "Path to server TLS certificate file")
	flag.StringVar(&cfg.CertFile, "certfile", "", "Path to client TLS certificate file")
	flag.StringVar(&cfg.KeyFile, "keyfile", "", "Path to client TLS private key file")
	flag.BoolVar(&cfg.TLS, "tls", false, "Enable TLS")
	logInit()
}

func main() {
	flag.Parse()
	var err error

	subOpts.Paths = gnmi.SplitPaths([]string{query})
	subOpts.StreamMode = "target_defined"
	subOpts.Mode = "stream"

	if *eosNative == true {
		subOpts.Origin = "eos_native"
	}

	ctx := gnmi.NewContext(context.Background(), cfg)
	client, err := gnmi.Dial(cfg)
	if err != nil {
		log.Fatalf("Failed to dial: %v", err)
	}

	respChan := make(chan *pb.SubscribeResponse, 10)
	errChan := make(chan error, 10)
	defer close(errChan)

	go func() {
		if err := gnmi.SubscribeErr(ctx, client, subOpts, respChan); err != nil {
			errChan <- err
		}
	}()

	firstPass := true

	for {
		select {
		case resp, open := <-respChan:
			if !open {
				// will read the error from the channel later
				continue
			}

			sync, err := collectResposes(resp)
			if err != nil {
				log.Fatal("Failed to collect responses: ", err)
			}
			if sync == true || firstPass == false {
				firstPass = false

				handleEvents(monitorMetrics)
			}
		case err := <-errChan:
			log.Fatal(err)
		case <-time.After(time.Second * time.Duration(*interval+1)):
			// if there are no changes in the interval period to response will be sent.
			// In this case assume the data has not changed
			handleEvents(monitorMetrics)
		}

	}
}

func transitionTriggerState(key string, raise bool, trigger_count uint, rearm_count uint) bool {

	if _, ok := triggerStates[key]; !ok {
		triggerStates[key] = &triggerState{}
	}

	s := triggerStates[key]

	switch raise {
	case true:
		return s.trigger(trigger_count)
	case false:
		return s.rearm(rearm_count)
	}

	return false
}

func collectResposes(response *pb.SubscribeResponse) (bool, error) {

	switch resp := response.Response.(type) {

	case *pb.SubscribeResponse_Error:
		return false, errors.New(resp.Error.Message)

	case *pb.SubscribeResponse_SyncResponse:
		if !resp.SyncResponse {
			return false, errors.New("initial sync failed")
		}
		return true, nil

	case *pb.SubscribeResponse_Update:
		timestamp := time.Unix(0, resp.Update.Timestamp).UTC().Format(time.RFC3339Nano)
		prefix := gnmi.StrPath(resp.Update.Prefix)

		for _, update := range resp.Update.Update {
			p := path.Join(prefix, gnmi.StrPath(update.Path))
			elems := strings.Split(p, "/")

			// get key from path
			key := elems[5]

			// get last elem
			last := elems[len(elems)-1]

			field, ok := getMonitorMetricFieldByAlias(monitorMetric{}, last)
			if ok != true {
				continue
			}
			if _, ok := monitorMetrics[key]; !ok {
				monitorMetrics[key] = &monitorMetric{
					Timestamp: timestamp,
				}
			}

			value, err := gnmi.ExtractValue(update)

			if err != nil {
				log.Printf("Failed to extract value: %s", err)
				continue
			}

			err = setMonitorMetricFieldByName(monitorMetrics[key], field, value)
			if err != nil {
				log.Printf("Failed to set value: %s", err)
				continue
			}
		}
	}

	return false, nil
}

func setMonitorMetricFieldByName(message *monitorMetric, field string, value interface{}) error {

	f := reflect.ValueOf(message).Elem().FieldByName(field)
	if f.IsValid() {
		switch v := value.(type) {
		case map[string]interface{}:
			f.Set(reflect.ValueOf(v["value"]))
		case float32, uint64, string:
			f.Set(reflect.ValueOf(v))
		default:
			return errors.New("Failed to set field")
		}

	}

	return nil
}

func getMonitorMetricFieldByAlias(message monitorMetric, alias string) (string, bool) {
	t := reflect.TypeOf(monitorMetric{})

	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		v := strings.Split(string(f.Tag), ":")[1]
		v = v[1 : len(v)-1]
		if v == alias {
			return f.Name, true
		}
	}

	return "", false
}

func handleEvents(metrics map[string]*monitorMetric) {

	for k, m := range metrics {
		setRTTBaseline(k, m.Latency)
		logRTTEvent(k, m)
		logLossEvent(k, m)
	}
}

func logRTTEvent(k string, m *monitorMetric) {
	if *rttThreshold == float64(0) {
		return
	}
	if m.Latency >= float32(*rttThreshold) {
		if transitionTriggerState(k+":rtt", true, *triggerCount, *rearmCount) {
			logRttTrigger(m.HostName, m.Latency, *rttThreshold)
		}
	} else if m.Latency < float32(*rttThreshold) {
		if transitionTriggerState(k+":rtt", false, *triggerCount, *rearmCount) {
			logRttRearm(m.HostName, m.Latency)
		}
	}
}

func logLossEvent(k string, m *monitorMetric) {

	if m.PacketLoss > *lossThreshold {
		if transitionTriggerState(k+":loss", true, *triggerCount, *rearmCount) {
			logLossTrigger(m.HostName, m.PacketLoss, lossThreshold)
		}
	} else if m.PacketLoss < *lossThreshold {
		if transitionTriggerState(k+":loss", false, *triggerCount, *rearmCount) {
			loglossRearm(m.HostName, m.PacketLoss)
		}
	}
}

func setRTTBaseline(k string, rtt float32) {
	if _, ok := rttBaselines[k]; !ok {
		logRttBaseline(k, rtt, float32(0))
		rttBaselines[k] = rtt
	}

	baseline := rttBaselines[k]
	hi := baseline * (1 + float32(*rttDeviationThreshold)/100)
	lo := baseline * (1 - float32(*rttDeviationThreshold)/100)

	if rtt <= lo {
		transitionTriggerState(k+":rtt_baseline_hi", false, *rttBaselineTrigger, 1)

		if transitionTriggerState(k+":rtt_baseline_lo", true, *rttBaselineTrigger, 0) {
			logRttBaseline(k, rtt, baseline)
			rttBaselines[k] = rtt
		}
	} else if rtt >= hi {
		transitionTriggerState(k+":rtt_baseline_lo", false, *rttBaselineTrigger, 1)

		if transitionTriggerState(k+":rtt_baseline_hi", true, *rttBaselineTrigger, 0) {
			logRttBaseline(k, rtt, baseline)
			rttBaselines[k] = rtt
		}
	} else {
		transitionTriggerState(k+":rtt_baseline_lo", false, *rttBaselineTrigger, 1)
		transitionTriggerState(k+":rtt_baseline_hi", false, *rttBaselineTrigger, 1)
	}

	log.Printf("%+v\n", triggerStates)
}
