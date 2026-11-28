package main

import "testing"

// rechargeTx next to the estimate it wraps, on a tx with a 6-hop path and
// decoded JSON (the hot call: once per pickBestObservation).
func BenchmarkRechargeTx_113(b *testing.B) {
	tx := &StoreTx{ID: 1, RawHex: "aabbccddeeff", Hash: "0123456789abcdef", DecodedJSON: `{"type":"GRP_TXT","channel":"#test","text":"hello world","sender":"node1"}`,
		PathJSON: `["01","02","03","04","05","06"]`}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if i%2 == 0 {
			tx.PathJSON = `["01","02","03","04","05","06"]`
		} else {
			tx.PathJSON = `["01","02","03","04","05","07"]`
		}
		tx.pathParsed = false
		rechargeTx(tx)
	}
}

func BenchmarkEstimateStoreTxBytesOnly_113(b *testing.B) {
	tx := &StoreTx{ID: 1, RawHex: "aabbccddeeff", Hash: "0123456789abcdef", DecodedJSON: `{"type":"GRP_TXT","channel":"#test","text":"hello world","sender":"node1"}`,
		PathJSON: `["01","02","03","04","05","06"]`}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if i%2 == 0 {
			tx.PathJSON = `["01","02","03","04","05","06"]`
		} else {
			tx.PathJSON = `["01","02","03","04","05","07"]`
		}
		tx.pathParsed = false
		estimateStoreTxBytes(tx)
	}
}
