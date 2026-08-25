package main

import "strings"

// channelHashHexKey is the JSON field name used for the wire-derived one-byte
// channel hash, both in the decoders' decoded_json payload and in the channel
// message objects the API emits. Shared so the in-memory and SQLite channel
// message paths cannot drift apart on spelling.
const channelHashHexKey = "channelHashHex"

// normalizeChannelHashHex validates and normalizes the on-wire one-byte
// channel hash carried by a decoded packet payload.
//
// The input is decoded_json's "channelHashHex". The ingestor decoder — the
// producer of every stored decoded_json — writes it as fmt.Sprintf("%02X",
// buf[0]) from the channel-hash byte of the GRP_TXT payload
// (cmd/ingestor/decoder.go), and carries it onto the decrypted CHAN payload.
// It is therefore derived from the packet itself. It is never derived from the
// channel name, from transmissions.channel_hash (which holds the
// operator-assigned channel name for decrypted messages), or from a channel
// key/PSK — and this helper deliberately has no access to any of those.
//
// The returned value is the canonical two-character uppercase form, including
// "00" and "FF". ok is false when the value is missing, empty, the wrong
// length, or not hexadecimal — including the "enc_<HEX>" channel-routing
// label, which is six characters and is not this field. A value that cannot
// be validated is reported as unavailable and is never guessed at, defaulted,
// or reconstructed from anything else.
//
// The result is one byte wide. It is protocol evidence, not a unique channel
// identity: distinct channels collide, so callers must not treat it as
// sufficient on its own to identify a channel.
func normalizeChannelHashHex(v interface{}) (string, bool) {
	s, ok := v.(string)
	if !ok || len(s) != 2 {
		return "", false
	}
	for i := 0; i < len(s); i++ {
		switch c := s[i]; {
		case c >= '0' && c <= '9':
		case c >= 'a' && c <= 'f':
		case c >= 'A' && c <= 'F':
		default:
			return "", false
		}
	}
	return strings.ToUpper(s), true
}

// setChannelHashHex copies the validated wire hash onto an outgoing channel
// message object. The key is left absent when the source value is missing or
// malformed, so a legacy row without the field stays absent rather than
// acquiring a placeholder.
func setChannelHashHex(data map[string]interface{}, v interface{}) {
	if hexVal, ok := normalizeChannelHashHex(v); ok {
		data[channelHashHexKey] = hexVal
	}
}
