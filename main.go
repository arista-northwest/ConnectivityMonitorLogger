// Copyright (c) 2019 Arista Networks, Inc.  All rights reserved.
// Arista Networks, Inc. Confidential and Proprietary.

package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"log/syslog"
	"os"
	"path"
	"reflect"
	"strings"
	"time"

	"github.com/aristanetworks/goarista/gnmi"
	pb "github.com/openconfig/gnmi/proto/gnmi"
)

const Path = "/Sysdb/connectivityMonitor/status/hostStatus"

var help = `Usage of ConnectivityMonitorLogger:
`

var TriggerCount uint
var RearmCount uint
var EventLogger *syslog.Writer
var RTTThreshold float64
var LossThreshold uint64

//var RTTBaselines map[string]*float32
var MonitorMetrics map[string]*MonitorMetric

type MonitorMetric struct {
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

// type Machine struct {
// 	current int32
// }

var TriggerStates map[string]*TriggerState

const (
	Armed = iota
	Triggered
	Rearming
	Triggering
)

type TriggerState struct {
	current int32
	offset  uint
}

func (t *TriggerState) trigger(max uint) int32 {
	if t.current == Triggered {
		return Triggered
	}
	t.offset += 1
	if t.offset >= max {
		t.current = Triggered
		t.offset = 0
		return Triggering
	}

	return Armed
}

func (t *TriggerState) rearm(max uint) int32 {
	if t.current == Armed {
		return Armed
	}
	t.offset += 1
	if t.offset >= max {
		t.current = Armed
		t.offset = 0
		return Rearming
	}

	return Triggered
}

func transitionTriggerState(key string, raise bool) int32 {

	if _, ok := TriggerStates[key]; !ok {
		TriggerStates[key] = &TriggerState{}
	}

	s := TriggerStates[key]

	switch raise {
	case true:
		return s.trigger(TriggerCount)
	case false:
		return s.rearm(RearmCount)
	}

	return s.current
}

func logEvents(metrics map[string]*MonitorMetric) {
	for k, m := range metrics {
		EventLogger.Debug(fmt.Sprintf("Received a message on %s: %+v", k, m))
		logRTTEvent(k, m)
		logLossEvent(k, m)
	}
}

func logRTTEvent(k string, m *MonitorMetric) {
	if m.Latency >= float32(RTTThreshold) {
		if transitionTriggerState(k+":rtt", true) == Triggering {
			EventLogger.Notice(fmt.Sprintf("RTT triggered event on %s rtt:%.2fms threshold:%.2fms", m.HostName, m.Latency, RTTThreshold))
		}
	} else if m.Latency < float32(RTTThreshold) {
		if transitionTriggerState(k+":rtt", false) == Rearming {
			EventLogger.Notice(fmt.Sprintf("RTT rearmed event on %s rtt:%.2fms", m.HostName, m.Latency))
		}
	}
}

func logLossEvent(k string, m *MonitorMetric) {
	if m.PacketLoss > LossThreshold {
		if transitionTriggerState(k+":loss", true) == Triggering {
			EventLogger.Notice(fmt.Sprintf("Packet loss triggered event on %s loss:%d%% threshold:%d%%", m.HostName, m.PacketLoss, LossThreshold))
		}
	} else if m.PacketLoss < LossThreshold {
		if transitionTriggerState(k+":loss", false) == Rearming {
			EventLogger.Notice(fmt.Sprintf("Packet loss rearmed event on %s loss:%d%%", m.HostName, m.PacketLoss))
		}
	}
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

			field, ok := getMonitorMetricFieldByAlias(MonitorMetric{}, last)
			if ok != true {
				continue
			}
			if _, ok := MonitorMetrics[key]; !ok {
				MonitorMetrics[key] = &MonitorMetric{
					Timestamp: timestamp,
				}
			}

			value, err := gnmi.ExtractValue(update)

			if err != nil {
				log.Printf("Failed to extract value: %s", err)
				continue
			}

			err = setMonitorMetricFieldByName(MonitorMetrics[key], field, value)
			if err != nil {
				log.Printf("Failed to set value: %s", err)
				continue
			}
		}
	}

	return false, nil
}

func setMonitorMetricFieldByName(message *MonitorMetric, field string, value interface{}) error {

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

func getMonitorMetricFieldByAlias(message MonitorMetric, alias string) (string, bool) {
	t := reflect.TypeOf(MonitorMetric{})

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

func main() {
	var err error
	cfg := &gnmi.Config{}
	subOpts := &gnmi.SubscribeOptions{}

	TriggerStates = make(map[string]*TriggerState)
	MonitorMetrics = make(map[string]*MonitorMetric)

	flag.StringVar(&cfg.Addr, "addr", "localhost:6030", "Sets the gNMI target (default is localhost:6030")
	flag.StringVar(&cfg.Username, "username", "admin", "Sets the gNMI user (default is admin)")
	flag.StringVar(&cfg.Password, "password", "", "Sets the gNMI password (default is <blank>)")

	flag.Uint64Var(&LossThreshold, "loss_threshold", 0.0, "Perect of lost packets to tolerate")
	flag.Float64Var(&RTTThreshold, "rtt_threshold", 10, "Percent deviation for RTT")
	flag.UintVar(&TriggerCount, "trigger_count", 3, "Number of events to trigger an alert (default is 3)")
	flag.UintVar(&RearmCount, "rearm_count", 2, "Number of events to rearm a previously triggered alert (default is 2)")
	eosNative := flag.Bool("eos_native", false, "set to true if 'eos_native' is enabled under 'management api gnmi'")
	interval := flag.Uint64("interval", 5, "Set to interval configured under 'monitor connectivity (default is 5")

	flag.Parse()

	subOpts.Paths = gnmi.SplitPaths([]string{Path})

	if *eosNative == true {
		subOpts.Origin = "eos_native"
	}

	flag.Usage = func() {
		fmt.Fprintln(os.Stderr, help)
		flag.PrintDefaults()
	}

	EventLogger, err = syslog.New(syslog.LOG_LOCAL4, "ConnectivityMonitorLogger")
	if err != nil {
		log.Fatal("Fail to setup syslogger")
	}

	log.SetOutput(EventLogger)

	ctx := gnmi.NewContext(context.Background(), cfg)
	client, err := gnmi.Dial(cfg)
	if err != nil {
		log.Fatal("Failed to connect: ", err)
	}

	respChan := make(chan *pb.SubscribeResponse, 10)
	errChan := make(chan error, 10)
	defer close(errChan)

	go gnmi.Subscribe(ctx, client, subOpts, respChan, errChan)

	firstPass := true

	for {
		select {
		case resp, open := <-respChan:
			if !open {
				log.Fatal("Failed to open a response channel")
			}

			sync, err := collectResposes(resp)
			if err != nil {
				log.Fatal("Failed to collect responses: ", err)
			}
			if sync == true || firstPass == false {
				firstPass = false

				logEvents(MonitorMetrics)
			}
		case err := <-errChan:
			log.Fatal("gRPC error received: ", err)
		case <-time.After(time.Second * time.Duration(*interval+1)):
			// if there are no changes in the interval period to response will be sent.
			// In this case assume the data has not changed
			logEvents(MonitorMetrics)
		}
	}
}
