package front

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"time"
)

const maxProxyProtocolLine = 108

// WriteProxyProtocolHeader forwards the original TCP endpoints to a private
// worker. Non-TCP or nil endpoints are encoded as PROXY UNKNOWN.
func WriteProxyProtocolHeader(writer io.Writer, source, destination net.Addr) error {
	sourceTCP, sourceOK := source.(*net.TCPAddr)
	destinationTCP, destinationOK := destination.(*net.TCPAddr)
	if !sourceOK || !destinationOK {
		_, err := io.WriteString(writer, "PROXY UNKNOWN\r\n")
		return err
	}
	family := "TCP6"
	if sourceTCP.IP.To4() != nil && destinationTCP.IP.To4() != nil {
		family = "TCP4"
	}
	_, err := fmt.Fprintf(writer, "PROXY %s %s %s %d %d\r\n",
		family, sourceTCP.IP.String(), destinationTCP.IP.String(), sourceTCP.Port, destinationTCP.Port)
	return err
}

type proxyProtocolListener struct {
	net.Listener
}

// NewProxyProtocolListener restores client addresses forwarded by Router.
// It must only wrap a private listener that accepts supervisor connections.
func NewProxyProtocolListener(listener net.Listener) net.Listener {
	return &proxyProtocolListener{Listener: listener}
}

func (l *proxyProtocolListener) Accept() (net.Conn, error) {
	for {
		connection, err := l.Listener.Accept()
		if err != nil {
			return nil, err
		}
		wrapped, err := readProxyProtocolHeader(connection)
		if err != nil {
			_ = connection.Close()
			continue
		}
		return wrapped, nil
	}
}

type proxyProtocolConn struct {
	net.Conn
	reader     *bufio.Reader
	remoteAddr net.Addr
	localAddr  net.Addr
}

func (c *proxyProtocolConn) Read(buffer []byte) (int, error) {
	return c.reader.Read(buffer)
}

func (c *proxyProtocolConn) RemoteAddr() net.Addr {
	return c.remoteAddr
}

func (c *proxyProtocolConn) LocalAddr() net.Addr {
	return c.localAddr
}

func readProxyProtocolHeader(connection net.Conn) (net.Conn, error) {
	_ = connection.SetReadDeadline(time.Now().Add(5 * time.Second))
	reader := bufio.NewReaderSize(connection, maxProxyProtocolLine+1)
	line, err := reader.ReadString('\n')
	_ = connection.SetReadDeadline(time.Time{})
	if err != nil {
		return nil, fmt.Errorf("read PROXY header: %w", err)
	}
	if len(line) > maxProxyProtocolLine {
		return nil, errors.New("PROXY header is too long")
	}
	fields := strings.Fields(strings.TrimSpace(line))
	if len(fields) == 2 && fields[0] == "PROXY" && fields[1] == "UNKNOWN" {
		return &proxyProtocolConn{
			Conn:       connection,
			reader:     reader,
			remoteAddr: connection.RemoteAddr(),
			localAddr:  connection.LocalAddr(),
		}, nil
	}
	if len(fields) != 6 || fields[0] != "PROXY" || (fields[1] != "TCP4" && fields[1] != "TCP6") {
		return nil, errors.New("invalid PROXY header")
	}
	remoteAddr, err := proxyTCPAddr(fields[2], fields[4])
	if err != nil {
		return nil, fmt.Errorf("invalid PROXY source: %w", err)
	}
	localAddr, err := proxyTCPAddr(fields[3], fields[5])
	if err != nil {
		return nil, fmt.Errorf("invalid PROXY destination: %w", err)
	}
	return &proxyProtocolConn{
		Conn:       connection,
		reader:     reader,
		remoteAddr: remoteAddr,
		localAddr:  localAddr,
	}, nil
}

func proxyTCPAddr(host, rawPort string) (*net.TCPAddr, error) {
	ip := net.ParseIP(host)
	if ip == nil {
		return nil, fmt.Errorf("invalid IP %q", host)
	}
	port, err := strconv.Atoi(rawPort)
	if err != nil || port < 0 || port > 65535 {
		return nil, fmt.Errorf("invalid port %q", rawPort)
	}
	return &net.TCPAddr{IP: ip, Port: port}, nil
}
