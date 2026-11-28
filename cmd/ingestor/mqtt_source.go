package main

import (
	"log"
	"time"

	mqtt "github.com/eclipse/paho.mqtt.golang"
)

// mqttSourceSetup is main()'s per-source setup up to the client itself.
type mqttSourceSetup struct {
	opts     *mqtt.ClientOptions  // with the connect, connection-lost and reconnecting handlers
	status   *sourceStatusState   // the source's status-registry entry
	liveness *SourceLivenessState // wired to the client by attachClient; registration is left to the caller
	secrets  []string             // mqttSourceSecrets, for every error the source logs
}

// prepareMQTTSource builds a source's mqttSourceSetup. The message handler
// needs the store and ingest buffer, so main() sets it.
func prepareMQTTSource(source MQTTSource, tag string) *mqttSourceSetup {
	logBroker := brokerForLog(source.Broker)
	secrets := mqttSourceSecrets(source)
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
	// in the stats file, so it gets the masked broker (#118).
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
		log.Printf("MQTT [%s] disconnected from %s: %s", tag, logBroker, errForLog(err, secrets...))
		status.MarkDisconnect(time.Now(), err, secrets...)
	})

	opts.SetReconnectingHandler(func(c mqtt.Client, options *mqtt.ClientOptions) {
		log.Printf("MQTT [%s] reconnecting to %s", tag, logBroker)
	})

	return &mqttSourceSetup{opts: opts, status: status, liveness: liveness, secrets: secrets}
}

// attachClient wires the source's liveness state to its client once that
// exists: the watchdog's connected check and its forced reconnect, which
// logs Connect()'s error with the source's secrets masked.
func (s *mqttSourceSetup) attachClient(client mqtt.Client) {
	s.liveness.IsConnectedFn = client.IsConnected
	s.liveness.ForceReconnectFn = buildForceReconnectFn(client, s.liveness.Tag, s.secrets...)
}
