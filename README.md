The mqtt-nats bridge enables [MQTT](http://mqtt.org/) devices to publish and subscribe to the [NATS](https://nats.io) communication system.

[![](https://img.shields.io/badge/License-Apache%202.0-blue.svg)](https://opensource.org/licenses/Apache-2.0)
[![](https://goreportcard.com/badge/github.com/tada/mqtt-nats)](https://goreportcard.com/report/github.com/tada/mqtt-nats)
[![](https://img.shields.io/badge/godoc-reference-blue.svg)](https://godoc.org/github.com/tada/mqtt-nats)
[![](https://github.com/tada/mqtt-nats/workflows/MQTT-NATS%20Test/badge.svg)](https://github.com/tada/mqtt-nats/actions)
[![](https://coveralls.io/repos/github/tada/mqtt-nats/badge.svg)](https://coveralls.io/github/tada/mqtt-nats)

### Project status
Currently under development.

### Current limitations:
- Only MQTT 3.1.1 is supported
- Only QoS levels 0 (at most once) and 1 (at least once)
- The bridge has no way of knowing when new subscriptions are added in the NATS network and hence, cannot send retained
messages in response to such subscriptions.

### QoS level 1 is accomplished using the NATS reply-to subject.
When the bridge receives an MQTT publish with QoS = 1 from a client, it forwards that to NATS with a reply-to subject.
A PUBACK is sent to the MQTT client when the reply arrives. Similarly, if an MQTT client subscribes using desired QoS
= 1, then a NATS publish with a reply-to will be considered in need of a PUBACK from the MQTT client.

### Retain request from NATS
An MQTT client that subscribes to a topic will immediately receive all retained messages for that topic. The same is
not true for a NATS client simply because the bridge has no way of knowing when a NATS client subscribes to a topic. To
mitigate this, the bridge subscribes to a specific "retained request" topic (configurable option). A NATS client that
publishes to this topic with a subscription string as the payload and a reply-to inbox, will get a JSON encoded reply
containing all messages that matches the subcription string.
