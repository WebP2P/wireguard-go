// +build !linux android

/* SPDX-License-Identifier: MIT
 *
 * Copyright (C) 2017-2019 WireGuard LLC. All Rights Reserved.
 */

package conn

import (
	"context"
	"fmt"
	"net"
	"os"
	"syscall"

	"github.com/tailscale/wireguard-go/wgcfg"
)

/* This code is meant to be a temporary solution
 * on platforms for which the sticky socket / source caching behavior
 * has not yet been implemented.
 *
 * See conn_linux.go for an implementation on the linux platform.
 */

type nativeBind struct {
	ipv4       *net.UDPConn
	ipv6       *net.UDPConn
	blackhole4 bool
	blackhole6 bool
}

type NativeEndpoint net.UDPAddr

var _ Bind = (*nativeBind)(nil)
var _ Endpoint = (*NativeEndpoint)(nil)

func CreateEndpoint(s string) (Endpoint, error) {
	addr, err := parseEndpoint(s)
	return (*NativeEndpoint)(addr), err
}

func (_ *NativeEndpoint) ClearSrc() {}

func (e *NativeEndpoint) DstIP() net.IP {
	return (*net.UDPAddr)(e).IP
}

func (e *NativeEndpoint) SrcIP() net.IP {
	return nil // not supported
}

func (e *NativeEndpoint) DstToBytes() []byte {
	addr := (*net.UDPAddr)(e)
	return addrToBytes(addr)
}

func addrToBytes(addr *net.UDPAddr) []byte {
	out := addr.IP.To4()
	if out == nil {
		out = addr.IP
	}
	out = append(out, byte(addr.Port&0xff))
	out = append(out, byte((addr.Port>>8)&0xff))
	return out
}

func (e *NativeEndpoint) DstToString() string {
	return (*net.UDPAddr)(e).String()
}

func (e *NativeEndpoint) SrcToString() string {
	return ""
}

func (e *NativeEndpoint) UpdateDst(dst *net.UDPAddr) error {
	*e = (NativeEndpoint)(*dst)
	return nil
}

func (e *NativeEndpoint) Addrs() []wgcfg.Endpoint {
	return []wgcfg.Endpoint{{
		Host: e.IP.String(),
		Port: uint16(e.Port),
	}}
}

func listenNet(network string, port int) (*net.UDPConn, int, error) {

	// listen
	ctx := context.Background()
	lc := net.ListenConfig{Control: netControl} // sets SO_REUSEADDR
	packetConn, err := lc.ListenPacket(ctx, network, fmt.Sprintf(":%d", port))
	if err != nil {
		return nil, 0, err
	}
	conn := packetConn.(*net.UDPConn)

	// retrieve port

	laddr := conn.LocalAddr()
	uaddr, err := net.ResolveUDPAddr(
		laddr.Network(),
		laddr.String(),
	)
	if err != nil {
		return nil, 0, err
	}
	return conn, uaddr.Port, nil
}

func extractErrno(err error) error {
	opErr, ok := err.(*net.OpError)
	if !ok {
		return nil
	}
	syscallErr, ok := opErr.Err.(*os.SyscallError)
	if !ok {
		return nil
	}
	return syscallErr.Err
}

func CreateBind(uport uint16, device interface{}) (Bind, uint16, error) {
	var err error
	var bind nativeBind

	port := int(uport)

	bind.ipv4, port, err = listenNet("udp4", port)
	if err != nil && extractErrno(err) != syscall.EAFNOSUPPORT {
		return nil, 0, err
	}

	bind.ipv6, port, err = listenNet("udp6", port)
	if err != nil && extractErrno(err) != syscall.EAFNOSUPPORT {
		bind.ipv4.Close()
		bind.ipv4 = nil
		return nil, 0, err
	}

	return &bind, uint16(port), nil
}

func (bind *nativeBind) Close() error {
	var err1, err2 error
	if bind.ipv4 != nil {
		err1 = bind.ipv4.Close()
	}
	if bind.ipv6 != nil {
		err2 = bind.ipv6.Close()
	}
	if err1 != nil {
		return err1
	}
	return err2
}

func (bind *nativeBind) LastMark() uint32 { return 0 }

func (bind *nativeBind) ReceiveIPv4(buff []byte) (int, Endpoint, *net.UDPAddr, error) {
	if bind.ipv4 == nil {
		return 0, nil, nil, syscall.EAFNOSUPPORT
	}
	n, addr, err := bind.ipv4.ReadFromUDP(buff)
	if addr != nil {
		addr.IP = addr.IP.To4()
	}
	return n, (*NativeEndpoint)(addr), addr, err
}

func (bind *nativeBind) ReceiveIPv6(buff []byte) (int, Endpoint, *net.UDPAddr, error) {
	if bind.ipv6 == nil {
		return 0, nil, nil, syscall.EAFNOSUPPORT
	}
	n, addr, err := bind.ipv6.ReadFromUDP(buff)
	return n, (*NativeEndpoint)(addr), addr, err
}

func (bind *nativeBind) Send(buff []byte, endpoint Endpoint) error {
	var err error
	nend := endpoint.(*NativeEndpoint)
	if nend.IP.To4() != nil {
		if bind.ipv4 == nil {
			return syscall.EAFNOSUPPORT
		}
		if bind.blackhole4 {
			return nil
		}
		_, err = bind.ipv4.WriteToUDP(buff, (*net.UDPAddr)(nend))
	} else {
		if bind.ipv6 == nil {
			return syscall.EAFNOSUPPORT
		}
		if bind.blackhole6 {
			return nil
		}
		_, err = bind.ipv6.WriteToUDP(buff, (*net.UDPAddr)(nend))
	}
	return err
}
