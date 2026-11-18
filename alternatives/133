package activation

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"strconv"
)

func Listen(ctx context.Context, network, address string, fallback func(context.Context, string, string) (net.Listener, error)) (net.Listener, error) {
	pid := os.Getenv("LISTEN_PID")
	if pid == "" || pid != strconv.Itoa(os.Getpid()) {
		return fallback(ctx, network, address)
	}
	if os.Getenv("LISTEN_FDS") != "1" {
		return nil, errors.New("socket activation requires exactly one listener")
	}
	file := os.NewFile(3, "systemd-listener")
	if file == nil {
		return nil, errors.New("socket activation listener is missing")
	}
	return inherit(file, network, address)
}

func inherit(file *os.File, network, address string) (net.Listener, error) {
	listener, err := net.FileListener(file)
	closeErr := file.Close()
	if err != nil {
		return nil, fmt.Errorf("inherit activated listener: %w", errors.Join(err, closeErr))
	}
	if closeErr != nil {
		return nil, errors.Join(closeErr, listener.Close())
	}
	actual, ok := listener.Addr().(*net.TCPAddr)
	host, port, err := net.SplitHostPort(address)
	wantPort, portErr := strconv.Atoi(port)
	if network != "tcp" || !ok || err != nil || portErr != nil || actual.Port != wantPort || !actual.IP.Equal(net.ParseIP(host)) {
		return nil, errors.Join(errors.New("activated listener does not match configured TCP address"), listener.Close())
	}
	return listener, nil
}
