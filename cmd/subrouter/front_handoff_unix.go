//go:build aix || android || darwin || dragonfly || freebsd || illumos || ios || linux || netbsd || openbsd || solaris

package main

import (
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"strconv"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

const (
	frontHandoffMarkerEnv = "SUBROUTER_FRONT_HANDOFF"
	frontHandoffTimeout   = 5 * time.Second

	frontHandoffPublicFD   = 3
	frontHandoffControlFD  = 4
	frontHandoffTransferFD = 5
	frontHandoffGateFD     = 6
	frontHandoffStatusFD   = 7

	frontHandoffPrepared = byte('P')
	frontHandoffCommit   = byte('C')
	frontHandoffReady    = byte('R')
	frontHandoffActivate = byte('A')
	frontHandoffStarted  = byte('S')
	frontHandoffOwn      = byte('O')
	frontHandoffRetire   = byte('X')
	frontHandoffServing  = byte('D')
)

var errFrontSuccessorExited = errors.New("front successor exited during handoff")

func frontProcessSignals() []os.Signal {
	return []os.Signal{os.Interrupt, syscall.SIGTERM, syscall.SIGHUP}
}

func isFrontReloadSignal(received os.Signal) bool {
	return received == syscall.SIGHUP
}

func inheritedFrontProcessFromEnvironment(config frontConfig) (*inheritedFrontProcess, error) {
	marker := os.Getenv(frontHandoffMarkerEnv)
	if marker == "" {
		return nil, nil
	}
	_ = os.Unsetenv(frontHandoffMarkerEnv)
	unsetSystemdListenEnv()
	if marker != "1" {
		return nil, fmt.Errorf("invalid %s value %q", frontHandoffMarkerEnv, marker)
	}
	publicListener, err := listenerFromInheritedFD(frontHandoffPublicFD, "front-handoff-public")
	if err != nil {
		return nil, err
	}
	controlListener, err := listenerFromInheritedFD(frontHandoffControlFD, "front-handoff-control")
	if err != nil {
		_ = publicListener.Close()
		return nil, err
	}
	transferListener, err := listenerFromInheritedFD(frontHandoffTransferFD, "front-handoff-transfer")
	if err != nil {
		closeFrontListeners(publicListener, controlListener)
		return nil, err
	}
	if _, ok := publicListener.(*net.TCPListener); !ok || !listenerAddressMatches(publicListener.Addr(), config.Addr) {
		actual := publicListener.Addr().String()
		closeFrontListeners(publicListener, controlListener, transferListener)
		return nil, fmt.Errorf("inherited front public listener %q does not match %q", actual, config.Addr)
	}
	if err := validateInheritedUnixListener(controlListener, config.ControlSocket, "control"); err != nil {
		closeFrontListeners(publicListener, controlListener, transferListener)
		return nil, err
	}
	if err := validateInheritedUnixListener(transferListener, config.ListenerTransferSocket, "transfer"); err != nil {
		closeFrontListeners(publicListener, controlListener, transferListener)
		return nil, err
	}
	disableAutomaticUnixUnlink(controlListener)
	disableAutomaticUnixUnlink(transferListener)
	gate := os.NewFile(frontHandoffGateFD, "front-handoff-gate")
	status := os.NewFile(frontHandoffStatusFD, "front-handoff-status")
	if gate == nil || status == nil {
		closeFrontListeners(publicListener, controlListener, transferListener)
		if gate != nil {
			_ = gate.Close()
		}
		if status != nil {
			_ = status.Close()
		}
		return nil, errors.New("front handoff synchronization descriptors are unavailable")
	}
	var closeOnce sync.Once
	closeSync := func() {
		closeOnce.Do(func() {
			_ = gate.Close()
			_ = status.Close()
		})
	}
	writeStatus := func(value byte) error {
		written, err := status.Write([]byte{value})
		if err != nil {
			return err
		}
		if written != 1 {
			return io.ErrShortWrite
		}
		return nil
	}
	waitFor := func(expected byte) error {
		value := []byte{0}
		if _, err := io.ReadFull(gate, value); err != nil {
			return err
		}
		if value[0] != expected {
			return fmt.Errorf("front handoff received marker %q, want %q", value[0], expected)
		}
		return nil
	}
	waitForOwnership := func() (bool, error) {
		value := []byte{0}
		if _, err := io.ReadFull(gate, value); err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
				return false, nil
			}
			return false, err
		}
		switch value[0] {
		case frontHandoffOwn:
			return true, nil
		case frontHandoffRetire:
			return false, nil
		default:
			return false, fmt.Errorf(
				"front handoff received ownership marker %q, want %q or %q",
				value[0], frontHandoffOwn, frontHandoffRetire,
			)
		}
	}
	return &inheritedFrontProcess{
		publicListener:    publicListener,
		controlListener:   controlListener,
		transferListener:  transferListener,
		prepared:          func() error { return writeStatus(frontHandoffPrepared) },
		waitForCommit:     func() error { return waitFor(frontHandoffCommit) },
		ready:             func() error { return writeStatus(frontHandoffReady) },
		waitForActivation: func() error { return waitFor(frontHandoffActivate) },
		started:           func() error { return writeStatus(frontHandoffStarted) },
		waitForOwnership:  waitForOwnership,
		serving:           func() error { return writeStatus(frontHandoffServing) },
		closeSync:         closeSync,
	}, nil
}

func listenerFromInheritedFD(descriptor int, name string) (net.Listener, error) {
	file := os.NewFile(uintptr(descriptor), name)
	if file == nil {
		return nil, fmt.Errorf("inherited listener fd %d is unavailable", descriptor)
	}
	listener, err := net.FileListener(file)
	closeErr := file.Close()
	if err != nil {
		return nil, fmt.Errorf("open inherited listener fd %d: %w", descriptor, err)
	}
	if closeErr != nil {
		_ = listener.Close()
		return nil, fmt.Errorf("close inherited listener fd %d: %w", descriptor, closeErr)
	}
	return listener, nil
}

func validateInheritedUnixListener(listener net.Listener, expected, purpose string) error {
	unixListener, ok := listener.(*net.UnixListener)
	if !ok || unixListener.Addr().Network() != "unix" || unixListener.Addr().String() != expected {
		return fmt.Errorf("inherited front %s listener %q does not match %q", purpose, listener.Addr(), expected)
	}
	return nil
}

type processFrontSuccessor struct {
	command *exec.Cmd
	gate    *os.File
	status  *os.File
	done    chan error
	once    sync.Once
}

func startFrontSuccessor(
	config frontConfig,
	publicListener net.Listener,
	controlListener net.Listener,
	transferListener net.Listener,
) (frontSuccessor, error) {
	if config.executable == "" {
		return nil, errors.New("front executable path is unavailable")
	}
	listenerFiles := make([]*os.File, 0, 3)
	for _, item := range []struct {
		listener net.Listener
		name     string
	}{
		{publicListener, "front-handoff-public"},
		{controlListener, "front-handoff-control"},
		{transferListener, "front-handoff-transfer"},
	} {
		file, err := duplicateFrontListenerFile(item.listener, item.name)
		if err != nil {
			closeFiles(listenerFiles...)
			return nil, err
		}
		listenerFiles = append(listenerFiles, file)
	}
	gateRead, gateWrite, err := os.Pipe()
	if err != nil {
		closeFiles(listenerFiles...)
		return nil, err
	}
	statusRead, statusWrite, err := os.Pipe()
	if err != nil {
		closeFiles(append(listenerFiles, gateRead, gateWrite)...)
		return nil, err
	}
	command := exec.Command(config.executable, frontSuccessorArgs(config)...)
	command.Env = append(os.Environ(), frontHandoffMarkerEnv+"=1")
	command.ExtraFiles = append(listenerFiles, gateRead, statusWrite)
	command.Stdout = os.Stdout
	command.Stderr = os.Stderr
	if err := command.Start(); err != nil {
		closeFiles(append(listenerFiles, gateRead, gateWrite, statusRead, statusWrite)...)
		return nil, err
	}
	closeFiles(append(listenerFiles, gateRead, statusWrite)...)
	successor := &processFrontSuccessor{
		command: command,
		gate:    gateWrite,
		status:  statusRead,
		done:    make(chan error, 1),
	}
	go func() { successor.done <- command.Wait() }()
	if err := successor.await(frontHandoffPrepared, config.ReadyTimeout); err != nil {
		successor.Abort()
		return nil, fmt.Errorf("wait for front successor preparation: %w", err)
	}
	return successor, nil
}

func frontSuccessorArgs(config frontConfig) []string {
	return []string{
		"front",
		"--addr", config.Addr,
		"--control-socket", config.ControlSocket,
		"--listener-transfer-socket", config.ListenerTransferSocket,
		"--backend-id", config.BackendID,
		"--backend-network", config.BackendNetwork,
		"--backend-address", config.BackendAddress,
		"--ready-timeout", config.ReadyTimeout.String(),
		"--drain-log-interval", config.DrainLogInterval.String(),
	}
}

func (s *processFrontSuccessor) PID() int {
	return s.command.Process.Pid
}

func (s *processFrontSuccessor) Commit(timeout time.Duration) error {
	if err := writeFrontHandoffMarker(s.gate, frontHandoffCommit); err != nil {
		return err
	}
	return s.await(frontHandoffReady, timeout)
}

func (s *processFrontSuccessor) Activate(timeout time.Duration) error {
	if err := writeFrontHandoffMarker(s.gate, frontHandoffActivate); err != nil {
		return err
	}
	if err := s.await(frontHandoffStarted, timeout); err != nil {
		return err
	}
	return nil
}

func (s *processFrontSuccessor) Confirm() (bool, error) {
	if err := writeFrontHandoffMarker(s.gate, frontHandoffOwn); err != nil {
		return false, err
	}
	err := s.await(frontHandoffServing, frontHandoffTimeout)
	if err != nil {
		committed := !errors.Is(err, io.EOF) &&
			!errors.Is(err, io.ErrUnexpectedEOF) &&
			!errors.Is(err, errFrontSuccessorExited)
		if committed {
			select {
			case exitErr := <-s.done:
				committed = false
				if exitErr == nil {
					err = errors.Join(err, errFrontSuccessorExited)
				} else {
					err = errors.Join(err, fmt.Errorf("%w: %v", errFrontSuccessorExited, exitErr))
				}
			default:
			}
		}
		s.once.Do(func() {
			_ = s.gate.Close()
			_ = s.status.Close()
		})
		return committed, err
	}
	s.once.Do(func() {
		_ = s.gate.Close()
		_ = s.status.Close()
	})
	return true, nil
}

func (s *processFrontSuccessor) Abort() {
	s.once.Do(func() {
		_ = s.gate.Close()
		_ = s.status.Close()
		_ = s.command.Process.Kill()
	})
}

func (s *processFrontSuccessor) Retire() {
	if err := writeFrontHandoffMarker(s.gate, frontHandoffRetire); err != nil {
		_ = s.command.Process.Signal(syscall.SIGTERM)
	}
	s.once.Do(func() {
		_ = s.gate.Close()
		_ = s.status.Close()
	})
}

func (s *processFrontSuccessor) await(expected byte, timeout time.Duration) error {
	type readResult struct {
		value byte
		err   error
	}
	result := make(chan readResult, 1)
	go func() {
		payload := []byte{0}
		_, err := io.ReadFull(s.status, payload)
		result <- readResult{value: payload[0], err: err}
	}()
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case read := <-result:
		if read.err != nil {
			return read.err
		}
		if read.value != expected {
			return fmt.Errorf("front successor sent marker %q, want %q", read.value, expected)
		}
		return nil
	case err := <-s.done:
		if err == nil {
			return errFrontSuccessorExited
		}
		return fmt.Errorf("%w: %v", errFrontSuccessorExited, err)
	case <-timer.C:
		return fmt.Errorf("front successor handoff timed out after %s", timeout)
	}
}

func writeFrontHandoffMarker(file *os.File, marker byte) error {
	written, err := file.Write([]byte{marker})
	if err != nil {
		return err
	}
	if written != 1 {
		return io.ErrShortWrite
	}
	return nil
}

func duplicateFrontListenerFile(listener net.Listener, name string) (*os.File, error) {
	if listener == nil {
		return nil, errors.New("front handoff listener is unavailable")
	}
	rawListener, ok := listener.(syscall.Conn)
	if !ok {
		return nil, fmt.Errorf("front handoff listener %s is %T, want syscall.Conn", name, listener)
	}
	rawConnection, err := rawListener.SyscallConn()
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
		return nil, errors.New("front handoff listener duplication returned an invalid descriptor")
	}
	unix.CloseOnExec(duplicate)
	file := os.NewFile(uintptr(duplicate), name)
	if file == nil {
		_ = unix.Close(duplicate)
		return nil, errors.New("duplicated front handoff listener is unavailable")
	}
	return file, nil
}

func closeFiles(files ...*os.File) {
	for _, file := range files {
		if file != nil {
			_ = file.Close()
		}
	}
}

func promoteFrontSuccessor(pid int) error {
	if pid <= 1 {
		return fmt.Errorf("front successor pid %d is invalid", pid)
	}
	notified, err := notifyFrontMainPID(strconv.Itoa(pid))
	if err != nil {
		return err
	}
	if !notified {
		return errors.New("front process manager does not support MAINPID handoff")
	}
	return nil
}
