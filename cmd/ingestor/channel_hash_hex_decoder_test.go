package main

import (
	"encoding/hex"
	"fmt"
	"testing"
)

// The ingestor decoder is the sole producer of the decoded_json that the
// server's channel-message API reads, so the exact formatting of the wire
// hash byte is an API contract, not just a decoder detail. These lock it
// independently of the pre-existing decoder tests.
//
// The value must always be exactly two uppercase hex characters, including a
// leading zero — the sibling numeric ChannelHash field is `omitempty` and
// silently drops a legitimate 0, which is why the hex string is the field the
// API contract is built on.

var wireHashBytes = []byte{0x00, 0x03, 0xA7, 0xFF}

func TestChannelHashHexContractUndecryptable(t *testing.T) {
	for _, b := range wireHashBytes {
		want := fmt.Sprintf("%02X", b)
		buf := []byte{b, 0xAA, 0xBB, 0xCC, 0xDD, 0xEE, 0xFF, 0x11, 0x22}
		p := decodeGrpTxt(buf, nil)

		if p.ChannelHashHex != want {
			t.Errorf("wire byte 0x%02X: ChannelHashHex = %q, want %q", b, p.ChannelHashHex, want)
		}
		if len(p.ChannelHashHex) != 2 {
			t.Errorf("wire byte 0x%02X: length %d, want exactly 2", b, len(p.ChannelHashHex))
		}
		if p.ChannelHash != int(b) {
			t.Errorf("wire byte 0x%02X: ChannelHash = %d, want %d", b, p.ChannelHash, int(b))
		}
	}
}

// A decrypted channel message must carry the same wire byte through onto the
// CHAN payload — that is the value the API surfaces, and it must never be
// replaced by anything derived from the channel name or key.
func TestChannelHashHexContractDecryptedCHAN(t *testing.T) {
	const key = "2cc3d22840e086105ad73443da2cacb8"
	ctHex, macHex := buildTestCiphertext(key, "Bob: Testing 123", 1700000000)
	macBytes, _ := hex.DecodeString(macHex)
	ctBytes, _ := hex.DecodeString(ctHex)

	for _, b := range wireHashBytes {
		want := fmt.Sprintf("%02X", b)
		buf := []byte{b}
		buf = append(buf, macBytes...)
		buf = append(buf, ctBytes...)

		p := decodeGrpTxt(buf, map[string]string{"#test": key})

		if p.Type != "CHAN" {
			t.Fatalf("wire byte 0x%02X: type = %s, want CHAN", b, p.Type)
		}
		if p.ChannelHashHex != want {
			t.Errorf("wire byte 0x%02X: ChannelHashHex = %q, want %q", b, p.ChannelHashHex, want)
		}
		// The channel name is the operator's configuration label and must
		// have no influence on the hash byte.
		if p.Channel != "#test" {
			t.Errorf("wire byte 0x%02X: channel = %q, want #test", b, p.Channel)
		}
		if p.SenderTimestamp != 1700000000 {
			t.Errorf("wire byte 0x%02X: senderTimestamp = %d, want 1700000000", b, p.SenderTimestamp)
		}
	}
}

// Two different channel names decrypting the same wire byte must both report
// that byte: one byte is collision-prone evidence, not a unique identity.
func TestChannelHashHexContractNameDoesNotAlterByte(t *testing.T) {
	const key = "2cc3d22840e086105ad73443da2cacb8"
	ctHex, macHex := buildTestCiphertext(key, "Bob: hi", 1700000000)
	macBytes, _ := hex.DecodeString(macHex)
	ctBytes, _ := hex.DecodeString(ctHex)

	buf := []byte{0xA7}
	buf = append(buf, macBytes...)
	buf = append(buf, ctBytes...)

	for _, name := range []string{"#test", "#completely-different-label"} {
		p := decodeGrpTxt(buf, map[string]string{name: key})
		if p.ChannelHashHex != "A7" {
			t.Errorf("channel name %q: ChannelHashHex = %q, want A7", name, p.ChannelHashHex)
		}
	}
}
