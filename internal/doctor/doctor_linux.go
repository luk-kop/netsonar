//go:build linux

package doctor

import (
	"fmt"

	"golang.org/x/net/icmp"
	"golang.org/x/sys/unix"
)

func DefaultEnv() Env {
	env := fillEnvDefaults(Env{})
	env.OpenUnprivilegedICMP = openUnprivilegedICMP
	env.CheckMTUPingSocket = checkMTUPingSocket
	return env
}

func openUnprivilegedICMP() error {
	conn, err := icmp.ListenPacket("udp4", "0.0.0.0")
	if err != nil {
		return err
	}
	_ = conn.Close()
	return nil
}

func checkMTUPingSocket() error {
	fd, err := unix.Socket(unix.AF_INET, unix.SOCK_DGRAM|unix.SOCK_CLOEXEC, unix.IPPROTO_ICMP)
	if err != nil {
		return err
	}
	defer func() { _ = unix.Close(fd) }()

	if err := unix.SetsockoptInt(fd, unix.IPPROTO_IP, unix.IP_RECVERR, 1); err != nil {
		return fmt.Errorf("set IP_RECVERR: %w", err)
	}
	if err := unix.SetsockoptInt(fd, unix.IPPROTO_IP, unix.IP_MTU_DISCOVER, unix.IP_PMTUDISC_PROBE); err != nil {
		return fmt.Errorf("set IP_MTU_DISCOVER: %w", err)
	}
	return nil
}
