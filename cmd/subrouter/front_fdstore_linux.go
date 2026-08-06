package main

import (
	"errors"
	"fmt"
	"net"
	"os"
	"strings"
	"sync/atomic"

	"golang.org/x/sys/unix"
)

var systemdNotifySocketSequence atomic.Uint64

func storeFrontListener(listener net.Listener) error {
	name, err := frontListenerStoreName(listener.Addr())
	if err != nil {
		return err
	}
	notified, err := notifySystemdDescriptorStore("FDSTOREREMOVE=1\nFDNAME="+name, nil)
	if err != nil || !notified {
		return err
	}
	tcpListener, ok := listener.(*net.TCPListener)
	if !ok {
		return fmt.Errorf("front descriptor store listener is %T, want TCP listener", listener)
	}
	file, err := duplicateTCPListenerFile(tcpListener)
	if err != nil {
		return fmt.Errorf("duplicate front listener for systemd descriptor store: %w", err)
	}
	defer file.Close()
	notified, err = notifySystemdDescriptorStore("FDSTORE=1\nFDNAME="+name, file)
	if err != nil {
		return err
	}
	if !notified {
		return errors.New("systemd notification socket disappeared while storing front listener")
	}
	return nil
}

func removeStoredFrontListener(address net.Addr) error {
	name, err := frontListenerStoreName(address)
	if err != nil {
		return err
	}
	_, err = notifySystemdDescriptorStore("FDSTOREREMOVE=1\nFDNAME="+name, nil)
	return err
}

func frontListenerStoreName(address net.Addr) (string, error) {
	port, err := frontListenerDescriptorStoreSlot(address)
	if err != nil {
		return "", fmt.Errorf("front listener address for descriptor store is invalid: %w", err)
	}
	return "subrouter-front-listener-" + port, nil
}

func notifySystemdDescriptorStore(message string, file *os.File) (bool, error) {
	path := os.Getenv("NOTIFY_SOCKET")
	if path == "" {
		return false, nil
	}
	if strings.HasPrefix(path, "@") {
		path = "\x00" + path[1:]
	} else if !strings.HasPrefix(path, "/") {
		return true, fmt.Errorf("systemd notification socket is not absolute or abstract")
	}
	address := &net.UnixAddr{Name: path, Net: "unixgram"}
	localAddress := &net.UnixAddr{
		Name: fmt.Sprintf("\x00subrouter-notify-%d-%d", os.Getpid(), systemdNotifySocketSequence.Add(1)),
		Net:  "unixgram",
	}
	connection, err := net.ListenUnixgram("unixgram", localAddress)
	if err != nil {
		return true, fmt.Errorf("open systemd notification socket: %w", err)
	}
	defer connection.Close()
	if file == nil {
		written, _, writeErr := connection.WriteMsgUnix([]byte(message), nil, address)
		if writeErr != nil {
			return true, fmt.Errorf("notify systemd descriptor store: %w", writeErr)
		}
		if written != len(message) {
			return true, errors.New("notify systemd descriptor store: short write")
		}
		return true, nil
	}
	written, controlWritten, err := connection.WriteMsgUnix(
		[]byte(message), unix.UnixRights(int(file.Fd())), address,
	)
	if err != nil {
		return true, fmt.Errorf("send listener to systemd descriptor store: %w", err)
	}
	if written != len(message) || controlWritten == 0 {
		return true, errors.New("send listener to systemd descriptor store: incomplete message")
	}
	return true, nil
}
