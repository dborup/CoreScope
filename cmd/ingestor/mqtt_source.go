package main

import (
	"log"
	"time"

	mqtt "github.com/eclipse/paho.mqtt.golang"
)

// prepareMQTTSource is main()'s per-source setup up to the client itself:
// the paho options with the connect, connection-lost and reconnecting
// handlers, the source's status-registry entry and its liveness state
// (IsConnectedFn and ForceReconnectFn are wired by the caller once the
// client exists; registration is left to the caller too). The message
// handler needs the store and ingest buffer, so main() sets it.
func prepareMQTTSource(source MQTTSource, tag string) (*mqtt.ClientOptions, *sourceStatusState, *SourceLivenessState) {
	logBroker := brokerForLog(source.Broker)
	opts := buildMQTTOpts(source)
	clientID := opts.ClientID

	// Pre-allocate the liveness pointer so OnConnect can reset its
	// stale-message clock on reconnect (PR #1216 r1 item 2).
	liveness := &SourceLivenessState{
		Tag:    tag,
		Broker: logBroker, // the watchdog logs it
	}

	// #1043: per-source status registry. Idempotent — repeated
	// registration across reconnects returns the same state so
	// counters accumulate across the process lifetime. It is published
	// in the stats file, so it gets the stripped broker (#118).
	status := RegisterSourceStatus(tag, logBroker)

	opts.SetOnConnectHandler(func(c mqtt.Client) {
		log.Print(mqttConnectedLogLine(tag, source.Broker, clientID))
		status.MarkConnect(time.Now())
		// PR #1216 r1 item 2: clear the stale LastMessageUnix from
		// before the outage so the watchdog doesn't immediately scream
		// "stalled for 2h". Also restarts the cold-start grace window
		// and clears the alert cooldown so a fresh stall edge can fire.
		liveness.MarkReconnected(time.Now())
		topics := source.Topics
		if len(topics) == 0 {
			topics = []string{"meshcore/#"}
		}
		for _, t := range topics {
			token := c.Subscribe(t, 0, nil)
			token.Wait()
			if token.Error() != nil {
				log.Printf("MQTT [%s] subscribe error for %s: %v", tag, t, token.Error())
			} else {
				log.Printf("MQTT [%s] subscribed to %s", tag, t)
			}
		}
	})

	opts.SetConnectionLostHandler(func(c mqtt.Client, err error) {
		log.Printf("MQTT [%s] disconnected from %s: %s", tag, logBroker, errForLog(err))
		status.MarkDisconnect(time.Now(), err)
	})

	opts.SetReconnectingHandler(func(c mqtt.Client, options *mqtt.ClientOptions) {
		log.Printf("MQTT [%s] reconnecting to %s", tag, logBroker)
	})

	return opts, status, liveness
}
