// Copyright (c) Tailscale Inc & contributors
// SPDX-License-Identifier: BSD-3-Clause

// Package batching implements a socket optimized for increased throughput.
package batching

import (
	"net/netip"

	"golang.org/x/net/ipv4"
	"golang.org/x/net/ipv6"
	"tailscale.com/net/packet"
	"tailscale.com/types/nettype"
)

var (
	// This acts as a compile-time check for our usage of ipv6.Message in
	// [Conn] for both IPv6 and IPv4 operations.
	_ ipv6.Message = ipv4.Message{}
)

// Conn is a [nettype.PacketConn] that provides batched i/o using
// platform-specific optimizations, e.g. {recv,send}mmsg & UDP GSO/GRO.
//
// Conn does not support single packet reads (see ReadFromUDPAddrPort docs). It
// is the caller's responsibility to use the appropriate read API where a
// [nettype.PacketConn] has been upgraded to support batched i/o.
//
// Conn originated from (and is still used by) magicsock where its API was
// strongly influenced by [wireguard-go/conn.Bind] constraints, namely
// wireguard-go's ownership of packet memory.
type Conn interface {
	nettype.PacketConn
	// ReadFromUDPAddrPort always returns an error, as UDP GRO is incompatible
	// with single packet reads. A single datagram may be multiple, coalesced
	// datagrams, and this API lacks the ability to pass that context.
	//
	// TODO: consider detaching Conn from [nettype.PacketConn]
	ReadFromUDPAddrPort([]byte) (int, netip.AddrPort, error)
	// ReadBatch reads messages from [Conn] into msgs. It returns the number of
	// messages the caller should evaluate for nonzero len, as a zero len
	// message may fall on either side of a nonzero.
	//
	// Each [ipv6.Message.OOB] must be sized to at least MinControlMessageSize().
	ReadBatch(msgs []ipv6.Message, flags int) (n int, err error)
	// WriteBatchTo writes buffs to addr.
	//
	// If geneve.VNI.IsSet(), then geneve is encoded into the space preceding
	// offset, and offset must equal [packet.GeneveFixedHeaderLength]. If
	// !geneve.VNI.IsSet() then the space preceding offset is ignored.
	//
	// len(buffs) must be <= batchSize supplied in TryUpgradeToConn().
	//
	// WriteBatchTo may return a [neterror.ErrUDPGSODisabled] error if UDP GSO
	// was disabled as a result of a send error.
	WriteBatchTo(buffs [][]byte, addr netip.AddrPort, geneve packet.GeneveHeader, offset int) error
}

// ShardedConn is a [Conn] whose receive queue is split into independent shards,
// so more than one goroutine can drain it.
//
// TERAPLANE FORK ADDITION. wireguard-go runs exactly one goroutine per
// conn.ReceiveFunc, and magicsock supplies one func per address family, so all
// inbound traffic for every peer funnels through a single goroutine. For a
// kernel UDP socket that is fine (the kernel does the queueing), but for a
// transport that hands packets over in userspace it is the throughput ceiling
// of the whole tunnel: measured on a 100G subnet router with 100 peers, 58.8%
// of all inbound datagrams were dropped on a full handoff queue while most
// cores sat idle.
//
// A Conn that implements ShardedConn gets one ReceiveFunc per shard, and
// therefore one wireguard-go receive goroutine per shard. Implementations must
// map a datagram to a shard by its SOURCE address so that a given peer's
// packets always land in the same shard: sharding must not reorder a peer's
// flow, which would spend WireGuard's replay window rather than buy anything.
type ShardedConn interface {
	Conn
	// RxShards reports the number of shards, which is fixed for the Conn's
	// lifetime and must be >= 1.
	RxShards() int
	// ReadBatchShard is [Conn.ReadBatch] restricted to one shard. The caller
	// runs at most one goroutine per shard, so an implementation may assume a
	// single reader per shard.
	ReadBatchShard(shard int, msgs []ipv6.Message, flags int) (n int, err error)
}
