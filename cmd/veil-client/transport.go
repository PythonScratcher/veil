package main

import (
	"encoding/binary"

	"golang.org/x/net/http2/hpack"
)

const chromeVer = "131"

// Header order follows Chrome's (fetch() POST / top-level navigation).
func (t *Transport) postFieldsFor(path string) []hpack.HeaderField {
	f := func(n, v string) hpack.HeaderField { return hpack.HeaderField{Name: n, Value: v} }
	return []hpack.HeaderField{
		f(":method", "POST"), f(":authority", t.sni), f(":scheme", "https"), f(":path", path),
		f("sec-ch-ua", `"Google Chrome";v="`+chromeVer+`", "Chromium";v="`+chromeVer+`", "Not_A Brand";v="24"`),
		f("sec-ch-ua-mobile", "?0"),
		f("sec-ch-ua-platform", `"Windows"`),
		f("user-agent", chromeUA),
		f("content-type", "application/octet-stream"),
		f("accept", "*/*"),
		f("origin", "https://"+t.sni),
		f("sec-fetch-site", "same-origin"),
		f("sec-fetch-mode", "cors"),
		f("sec-fetch-dest", "empty"),
		f("referer", "https://"+t.sni+"/"),
		f("accept-encoding", "gzip, deflate, br, zstd"),
		f("accept-language", "en-US,en;q=0.9"),
		f("priority", "u=1, i"),
	}
}

func (t *Transport) getFieldsFor(path string) []hpack.HeaderField {
	f := func(n, v string) hpack.HeaderField { return hpack.HeaderField{Name: n, Value: v} }
	return []hpack.HeaderField{
		f(":method", "GET"), f(":authority", t.sni), f(":scheme", "https"), f(":path", path),
		f("sec-ch-ua", `"Google Chrome";v="`+chromeVer+`", "Chromium";v="`+chromeVer+`", "Not_A Brand";v="24"`),
		f("sec-ch-ua-mobile", "?0"),
		f("sec-ch-ua-platform", `"Windows"`),
		f("upgrade-insecure-requests", "1"),
		f("user-agent", chromeUA),
		f("accept", "text/html,application/xhtml+xml,application/xml;q=0.9,image/avif,image/webp,image/apng,*/*;q=0.8,application/signed-exchange;v=b3;q=0.7"),
		f("sec-fetch-site", "none"),
		f("sec-fetch-mode", "navigate"),
		f("sec-fetch-user", "?1"),
		f("sec-fetch-dest", "document"),
		f("accept-encoding", "gzip, deflate, br, zstd"),
		f("accept-language", "en-US,en;q=0.9"),
		f("priority", "u=0, i"),
	}
}

const chromeUA = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/" + chromeVer + ".0.0.0 Safari/537.36"

func (t *Transport) postFields() []hpack.HeaderField { return t.postFieldsFor(t.path) }
func (t *Transport) getFields() []hpack.HeaderField  { return t.getFieldsFor("/") }

func hf(n, v string) hpack.HeaderField { return hpack.HeaderField{Name: n, Value: v} }

func putU64(b []byte, v uint64) { binary.BigEndian.PutUint64(b, v) }
