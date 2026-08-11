# Why this fork exists

This is upstream tailscale.com at commit `cd34d441b` (v1.103.0-pre) plus a
small patch series (~150 changed lines across 9 files, enumerated below), on
branch `teraplane`. It exists for one reason: **Teraplane** (a userspace Go forwarding plane,
github.com/atoonk/go-vpp-poc) terminates Tailscale entirely inside its own
dataplane, and upstream exposes a seam for only half of that.

Tailscale's `tsnet.Server.Tun` field already lets an embedder supply the
device that carries DECRYPTED packets. That is supported, upstream, untouched
here. But the ENCRYPTED packets (the WireGuard/disco UDP to and from peers)
always ride a kernel UDP socket, with no way to substitute it. For a router
whose entire point is that packets never enter the kernel network stack, that
kernel socket is the last kernel dependency in the datapath, and on a NIC the
dataplane also taps, the router reads back its own ciphertext as foreign
traffic. The fork removes that last dependency.

Measured effect of the whole integration (both patches, on 10G ixgbe
hardware, subnet-router pairs, details in go-vpp-poc's
docs/RESULTS-TAILSCALE-HEAD2HEAD.md): ~2x a stock kernel-mode tailscaled pair
on one TCP flow, 4-5x on eight flows (8.4 Gbit/s, NIC-bound), with ~3x fewer
syscalls per gigabyte on the router.

## Patch 1: `PacketListener` on tsnet.Server and wgengine.Config

Files: `tsnet/tsnet.go` (+13/-2), `wgengine/userspace.go` (+12).

Adds one optional field at each layer and passes it down:

    tsnet.Server.PacketListener        (the embedder-facing knob)
      -> wgengine.Config.PacketListener  (the hop tsnet uses to build the engine)
        -> magicsock.Options.TestOnlyPacketListener  (the existing seam)

Why every layer is required:

- `magicsock` owns the UDP sockets and ALREADY has the seam
  (`Options.TestOnlyPacketListener`, consulted by its `listenPacket` for the
  udp4/udp6 binds). Zero magicsock lines are changed.
- `wgengine.Config` is the only way tsnet constructs magicsock; without the
  field there, nothing an embedder can reach connects to that seam.
- `tsnet.Server` is the supported embedding API (registration, netmap, DERP,
  key rotation, state, route approval all come with it). The alternative to
  this field is abandoning tsnet and reimplementing its control-plane setup
  against raw wgengine, which is a far larger and far more fragile fork.

`nil` means exactly upstream behavior: the ordinary kernel socket. Teraplane
sets it to a listener whose conns are backed by its dataplane (a claimed UDP
port on ingress, its forwarding graph on egress).

Known fragility, on purpose: the final assignment rides an upstream field
named `TestOnlyPacketListener`. It is the only existing seam; blessing a
production-named field is upstream's call, not a fork's. If upstream renames
or removes the hook, the rebase fails loudly at that line, which is the
desired failure mode. A proper upstream submission would propose a supported
field (precedent: `tsnet.Server.Tun` is exactly this class of seam) plus a
tsnet test with a fake listener.

## Patch 2: `net/batching` honors a conn that already batches

File: `net/batching/conn_linux.go` (+5).

`TryUpgradeToConn` upgraded only `*net.UDPConn` to batched i/o. Any other
transport, including one that implements `batching.Conn` natively, silently
fell back to one datagram per read and per write. For Teraplane's transport
that fallback cost 26% at four flows and 15% at eight (wireguard-go hands
sends in batches of up to 128; unbatched, each was a separate queue handoff).
The patch returns an already-batching conn as-is.

This is the upstreamable half, useful to anyone with a custom transport. A
submission should also: hoist the check above the platform/old-kernel gates
(those exist for the kernel-socket upgrade, not for a conn that already
batches), mirror it in `conn_default.go` so non-Linux behaves the same,
document that `batchSize` is advisory for an already-batching conn, and add a
passthrough test.

## Patch 3: one receive goroutine per transport shard

Files: `wgengine/magicsock/magicsock.go`, `wgengine/magicsock/rebinding_conn.go`
(+ test).

Upstream drains each family's UDP socket with ONE `ReceiveFunc` goroutine,
which is a single-core ceiling on a many-core box: measured on a 48-core
router, 21.6M of 30.4M offered packets per second were dropped while the box
sat mostly idle. If the underlying conn reports `RxShards() > 1` (Teraplane's
dataplane transport does; a kernel socket reports 1, keeping upstream
behavior), `connBind.Open` starts one receive goroutine per shard, each
reading its own queue via `ReadBatchShard`.

Notes an upstream submission would need: only shard 0 carries the
`health.ReceiveFuncStats` item (the stats type is a single Enter/Exit slot,
so shards sharing one would corrupt liveness state); shards past 0 therefore
get their own exit-error log line so a dying shard is never silent. After a
rebind to a non-sharded conn, all shard goroutines fall through to the plain
`ReadBatch`, which serializes on the conn's read mutex: correct, just no
longer parallel.

## Patch 4: `SetWGKeypairFunc`, observing transport keypairs

Files: `wgengine/userspace.go` (plumbing to wireguard-go's
`device.SetKeypairFunc`).

The seam that makes an external inline-decrypt fast path possible: the
embedder is told when a transport keypair is established, promoted, or
retired, and receives the `*device.Keypair` so it can maintain its own
index->keypair table. All safety obligations live in the wireguard-go fork
(github.com/atoonk/teraplane-wireguard-go): keypairs handed to an observer are
marked shared BEFORE they are reachable by the receive path, and replay
validation serializes on a per-keypair mutex from then on. This patch is
plumbing only; it holds no key material and adds no crypto.

## Maintenance

- Consumed by go-vpp-poc as the published module
  `github.com/atoonk/teraplane-tailscale` (branch `teraplane`), pinned by
  pseudo-version. The patches are also carried inside that repo
  (`third_party/tailscale-patches/`, format-patch form), so this fork is
  reproducible from any clean upstream checkout:
  `git checkout cd34d441b && git am third_party/tailscale-patches/*.patch`.
  The patch record must be re-exported from this branch's HEAD after every
  change here; an out-of-date record silently rebuilds an older fork.
- Upgrade policy: rebase this branch onto each upstream release the
  integration tracks (never merge), re-run go-vpp-poc's tsconn/engine tests
  and the hardware benchmark, then re-export the patch files.
- Every changed line carries a `Teraplane patch` comment, so
  `grep -rn "Teraplane patch"` enumerates the fork's full code surface.
