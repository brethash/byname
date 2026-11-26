package main

import (
	"crypto/tls"
	"encoding/binary"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// The public :443 listener tells HTTP/2 clients they may open at most
// maxStreamsPerConn streams per connection, so the connection limits bound
// requests too.
func TestPublicTLSCapsHTTP2Streams(t *testing.T) {
	donor := httptest.NewTLSServer(http.NotFoundHandler()) // only for its test certificate
	cert := donor.TLS.Certificates[0]
	donor.Close()

	srv := newHTTPServer(http.NotFoundHandler(), quiet)
	// The same protocols autocert's TLS config offers.
	configurePublicTLS(srv, &tls.Config{Certificates: []tls.Certificate{cert}, NextProtos: []string{"h2", "http/1.1", "acme-tls/1"}}, quiet)
	if srv.TLSConfig.MinVersion != tls.VersionTLS12 {
		t.Errorf("MinVersion = %x", srv.TLSConfig.MinVersion)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go srv.ServeTLS(ln, "", "")
	defer srv.Close()

	conn, err := tls.Dial("tcp", ln.Addr().String(), &tls.Config{InsecureSkipVerify: true, NextProtos: []string{"h2"}})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if p := conn.ConnectionState().NegotiatedProtocol; p != "h2" {
		t.Fatalf("negotiated %q, want h2", p)
	}
	// Client preface and an empty SETTINGS frame; the server's first frame
	// is its SETTINGS.
	conn.Write([]byte("PRI * HTTP/2.0\r\n\r\nSM\r\n\r\n"))
	conn.Write([]byte{0, 0, 0, 0x4, 0, 0, 0, 0, 0})
	conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	for {
		var hdr [9]byte
		if _, err := io.ReadFull(conn, hdr[:]); err != nil {
			t.Fatal(err)
		}
		length := int(hdr[0])<<16 | int(hdr[1])<<8 | int(hdr[2])
		payload := make([]byte, length)
		if _, err := io.ReadFull(conn, payload); err != nil {
			t.Fatal(err)
		}
		if hdr[3] != 0x4 || hdr[4]&0x1 != 0 { // not SETTINGS, or an ACK
			continue
		}
		for i := 0; i+6 <= len(payload); i += 6 {
			if binary.BigEndian.Uint16(payload[i:]) == 0x3 { // SETTINGS_MAX_CONCURRENT_STREAMS
				if v := binary.BigEndian.Uint32(payload[i+2:]); v != maxStreamsPerConn {
					t.Fatalf("MAX_CONCURRENT_STREAMS = %d, want %d", v, maxStreamsPerConn)
				}
				return
			}
		}
		t.Fatal("server SETTINGS has no MAX_CONCURRENT_STREAMS")
	}
}
