//go:build aix || android || darwin || dragonfly || freebsd || illumos || ios || linux || netbsd || openbsd || solaris

package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

const (
	maxListenerTransferBytes = 4 << 10
	listenerTransferTimeout  = 5 * time.Second
)

type listenerTransferRequest struct {
	Address string `json:"address"`
}

type listenerTransferResponse struct {
	Address string `json:"address,omitempty"`
	Error   string `json:"error,omitempty"`
}

func listenForTransferredListeners(path string) (net.Listener, error) {
	address, err := net.ResolveUnixAddr("unix", path)
	if err != nil {
		return nil, err
	}
	return net.ListenUnix("unix", address)
}

func (f *stableFront) serveListenerTransfers(listener net.Listener) error {
	unixListener, ok := listener.(*net.UnixListener)
	if !ok {
		return fmt.Errorf("listener transfer endpoint is %T, want Unix listener", listener)
	}
	for {
		connection, err := unixListener.AcceptUnix()
		if err != nil {
			return err
		}
		requestErr := f.handleListenerTransfer(connection)
		_ = connection.Close()
		if requestErr != nil {
			continue
		}
	}
}

func (f *stableFront) handleListenerTransfer(connection *net.UnixConn) error {
	if err := connection.SetDeadline(time.Now().Add(listenerTransferTimeout)); err != nil {
		return err
	}
	listener, err := receiveTransferredListener(connection)
	response := listenerTransferResponse{}
	if err == nil {
		err = f.replacePublicListener(listener)
	}
	if err != nil {
		response.Error = err.Error()
	} else {
		response.Address = listener.Addr().String()
	}
	marshalErr := json.NewEncoder(connection).Encode(response)
	if err != nil {
		return err
	}
	return marshalErr
}

func receiveTransferredListener(connection *net.UnixConn) (net.Listener, error) {
	payload := make([]byte, 1)
	control := make([]byte, unix.CmsgSpace(4*4))
	payloadBytes, controlBytes, flags, _, err := connection.ReadMsgUnix(payload, control)
	if err != nil {
		return nil, err
	}
	messages, err := unix.ParseSocketControlMessage(control[:controlBytes])
	if err != nil {
		return nil, fmt.Errorf("parse listener transfer control message: %w", err)
	}
	rights := make([]int, 0, 1)
	for _, message := range messages {
		descriptors, parseErr := unix.ParseUnixRights(&message)
		if parseErr != nil {
			for _, descriptor := range rights {
				_ = unix.Close(descriptor)
			}
			return nil, fmt.Errorf("parse transferred listener descriptor: %w", parseErr)
		}
		rights = append(rights, descriptors...)
	}
	for _, descriptor := range rights {
		unix.CloseOnExec(descriptor)
	}
	if payloadBytes != 1 || payload[0] != 1 || flags&(unix.MSG_TRUNC|unix.MSG_CTRUNC) != 0 {
		for _, descriptor := range rights {
			_ = unix.Close(descriptor)
		}
		return nil, errors.New("invalid listener transfer protocol marker")
	}
	if len(rights) != 1 {
		for _, descriptor := range rights {
			_ = unix.Close(descriptor)
		}
		return nil, fmt.Errorf("listener transfer supplied %d descriptors, want one", len(rights))
	}
	file := os.NewFile(uintptr(rights[0]), "subrouter-transferred-public-listener")
	if file == nil {
		_ = unix.Close(rights[0])
		return nil, errors.New("transferred listener descriptor is unavailable")
	}
	defer file.Close()

	requestBody, err := bufio.NewReaderSize(io.LimitReader(connection, maxListenerTransferBytes+1), maxListenerTransferBytes+1).ReadBytes('\n')
	if err != nil {
		return nil, fmt.Errorf("read listener transfer request: %w", err)
	}
	if len(requestBody) > maxListenerTransferBytes {
		return nil, errors.New("listener transfer request is too large")
	}
	decoder := json.NewDecoder(strings.NewReader(string(requestBody)))
	decoder.DisallowUnknownFields()
	var request listenerTransferRequest
	if err := decoder.Decode(&request); err != nil {
		return nil, fmt.Errorf("decode listener transfer request: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return nil, errors.New("decode listener transfer request: trailing JSON value")
	}
	if err := validateNumericTCPAddress(request.Address); err != nil {
		return nil, fmt.Errorf("invalid listener transfer address: %w", err)
	}
	listener, err := net.FileListener(file)
	if err != nil {
		return nil, fmt.Errorf("open transferred listener: %w", err)
	}
	if _, ok := listener.(*net.TCPListener); !ok || !listenerCoversConfiguredAddress(listener, request.Address) {
		actual := listener.Addr().String()
		_ = listener.Close()
		return nil, fmt.Errorf("transferred listener address %q does not match %q", actual, request.Address)
	}
	return listener, nil
}

func sendTransferredListener(socket, address string, listener net.Listener) error {
	tcpListener, ok := listener.(*net.TCPListener)
	if !ok {
		return fmt.Errorf("listener transfer source is %T, want TCP listener", listener)
	}
	file, err := duplicateTCPListenerFile(tcpListener)
	if err != nil {
		return fmt.Errorf("duplicate listener transfer source: %w", err)
	}
	defer file.Close()
	remote, err := net.ResolveUnixAddr("unix", socket)
	if err != nil {
		return err
	}
	connection, err := net.DialUnix("unix", nil, remote)
	if err != nil {
		return fmt.Errorf("connect listener transfer socket: %w", err)
	}
	defer connection.Close()
	if err := connection.SetDeadline(time.Now().Add(listenerTransferTimeout)); err != nil {
		return err
	}
	written, controlWritten, err := connection.WriteMsgUnix([]byte{1}, unix.UnixRights(int(file.Fd())), nil)
	if err != nil {
		return fmt.Errorf("send listener transfer: %w", err)
	}
	if written != 1 || controlWritten == 0 {
		return errors.New("send listener transfer: incomplete message")
	}
	if err := json.NewEncoder(connection).Encode(listenerTransferRequest{Address: address}); err != nil {
		return fmt.Errorf("send listener transfer request: %w", err)
	}
	responseBody, err := bufio.NewReaderSize(io.LimitReader(connection, maxListenerTransferBytes+1), maxListenerTransferBytes+1).ReadBytes('\n')
	if err != nil {
		return fmt.Errorf("read listener transfer response: %w", err)
	}
	if len(responseBody) == 0 || len(responseBody) > maxListenerTransferBytes {
		return errors.New("invalid listener transfer response size")
	}
	var response listenerTransferResponse
	decoder := json.NewDecoder(strings.NewReader(string(responseBody)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&response); err != nil {
		return fmt.Errorf("decode listener transfer response: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return errors.New("decode listener transfer response: trailing JSON value")
	}
	if response.Error != "" {
		return errors.New(response.Error)
	}
	if !listenerAddressMatches(listener.Addr(), response.Address) {
		return fmt.Errorf("listener transfer response address %q does not match %q", response.Address, listener.Addr())
	}
	return nil
}

func duplicateTCPListenerFile(listener *net.TCPListener) (*os.File, error) {
	rawConnection, err := listener.SyscallConn()
	if err != nil {
		return nil, err
	}
	duplicate := -1
	var duplicateErr error
	if err := rawConnection.Control(func(descriptor uintptr) {
		duplicate, duplicateErr = unix.Dup(int(descriptor))
	}); err != nil {
		return nil, err
	}
	if duplicateErr != nil {
		return nil, duplicateErr
	}
	if duplicate < 0 {
		return nil, errors.New("listener descriptor duplication returned an invalid descriptor")
	}
	unix.CloseOnExec(duplicate)
	file := os.NewFile(uintptr(duplicate), "subrouter-duplicated-tcp-listener")
	if file == nil {
		_ = unix.Close(duplicate)
		return nil, errors.New("duplicated listener descriptor is unavailable")
	}
	return file, nil
}

func runListenerTransfer(args []string) error {
	flags := flag.NewFlagSet("listener-transfer", flag.ContinueOnError)
	socket := flags.String("socket", "", "permissioned front listener transfer socket")
	address := flags.String("address", "", "expected TCP listener address")
	sourcePID := flags.Int("source-pid", 0, "process that owns the live TCP listener")
	sourceFD := flags.Int("source-fd", -1, "live TCP listener descriptor in --source-pid")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("listener-transfer takes no positional arguments")
	}
	if !filepath.IsAbs(*socket) {
		return fmt.Errorf("socket must be an absolute path, got %q", *socket)
	}
	if err := validateNumericTCPAddress(*address); err != nil {
		return fmt.Errorf("invalid listener address: %w", err)
	}
	listener, err := takeoverTCPListener(*sourcePID, *sourceFD, *address)
	if err != nil {
		return err
	}
	defer listener.Close()
	return sendTransferredListener(*socket, *address, listener)
}
