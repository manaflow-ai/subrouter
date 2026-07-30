package front

import (
	"bufio"
	"fmt"
	"net"
	"testing"
	"time"
)

func TestProxyProtocolListenerRestoresAddressesAndPayload(t *testing.T) {
	base, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	listener := NewProxyProtocolListener(base)
	t.Cleanup(func() { _ = listener.Close() })

	accepted := make(chan net.Conn, 1)
	errs := make(chan error, 1)
	go func() {
		connection, acceptErr := listener.Accept()
		if acceptErr != nil {
			errs <- acceptErr
			return
		}
		accepted <- connection
	}()

	client, err := net.Dial("tcp", listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	_, _ = fmt.Fprint(client, "PROXY TCP4 192.0.2.10 198.51.100.20 43210 31415\r\nhello\n")

	var server net.Conn
	select {
	case err := <-errs:
		t.Fatal(err)
	case server = <-accepted:
	case <-time.After(time.Second):
		t.Fatal("listener did not accept PROXY connection")
	}
	defer server.Close()
	if got := server.RemoteAddr().String(); got != "192.0.2.10:43210" {
		t.Fatalf("remote address = %q", got)
	}
	if got := server.LocalAddr().String(); got != "198.51.100.20:31415" {
		t.Fatalf("local address = %q", got)
	}
	payload, err := bufio.NewReader(server).ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	if payload != "hello\n" {
		t.Fatalf("payload = %q", payload)
	}
}

func TestProxyProtocolListenerRejectsMissingHeader(t *testing.T) {
	server, client := net.Pipe()
	defer client.Close()
	go func() {
		_, _ = fmt.Fprint(client, "GET / HTTP/1.1\r\n")
	}()
	if _, err := readProxyProtocolHeader(server); err == nil {
		t.Fatal("expected missing PROXY header to fail")
	}
}

func TestProxyProtocolListenerSkipsMalformedConnection(t *testing.T) {
	base, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	listener := NewProxyProtocolListener(base)
	t.Cleanup(func() { _ = listener.Close() })

	accepted := make(chan net.Conn, 1)
	errs := make(chan error, 1)
	go func() {
		connection, acceptErr := listener.Accept()
		if acceptErr != nil {
			errs <- acceptErr
			return
		}
		accepted <- connection
	}()

	malformed, err := net.Dial("tcp", listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	_, _ = fmt.Fprint(malformed, "GET / HTTP/1.1\r\n")
	_ = malformed.Close()

	valid, err := net.Dial("tcp", listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer valid.Close()
	_, _ = fmt.Fprint(valid, "PROXY TCP4 192.0.2.10 198.51.100.20 43210 31415\r\n")

	select {
	case err := <-errs:
		t.Fatal(err)
	case connection := <-accepted:
		defer connection.Close()
		if got := connection.RemoteAddr().String(); got != "192.0.2.10:43210" {
			t.Fatalf("remote address = %q", got)
		}
	case <-time.After(time.Second):
		t.Fatal("listener did not continue after malformed connection")
	}
}
