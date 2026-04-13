//go:build linux

package doctor

import (
	"fmt"
	"net"
	"syscall"

	"golang.org/x/net/icmp"
)

func DefaultEnv() Env {
	env := fillEnvDefaults(Env{})
	env.OpenUnprivilegedICMP = openUnprivilegedICMP
	env.CheckRawICMPPMTUProbe = checkRawICMPPMTUProbe
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

func checkRawICMPPMTUProbe() error {
	rawPC, err := net.ListenPacket("ip4:icmp", "0.0.0.0")
	if err != nil {
		return err
	}
	defer func() { _ = rawPC.Close() }()

	sc, ok := rawPC.(interface {
		SyscallConn() (syscall.RawConn, error)
	})
	if !ok {
		return fmt.Errorf("packet conn does not support SyscallConn")
	}
	rawConn, err := sc.SyscallConn()
	if err != nil {
		return fmt.Errorf("get raw conn: %w", err)
	}

	var sockErr error
	err = rawConn.Control(func(fd uintptr) {
		sockErr = syscall.SetsockoptInt(int(fd), syscall.IPPROTO_IP, syscall.IP_MTU_DISCOVER, syscall.IP_PMTUDISC_PROBE)
	})
	if err != nil {
		return fmt.Errorf("raw conn control: %w", err)
	}
	if sockErr != nil {
		return fmt.Errorf("set IP_MTU_DISCOVER: %w", sockErr)
	}
	return nil
}
