/* SPDX-License-Identifier: MIT
 *
 * Copyright (C) 2017-2019 WireGuard LLC. All Rights Reserved.
 *
 * This is based heavily on timers.c from the kernel implementation.
 */

package device

import (
	"math/rand"
	"sync"
	"sync/atomic"
	"time"
)

/* This Timer structure and related functions should roughly copy the interface of
 * the Linux kernel's struct timer_list.
 */

type Timer struct {
	*time.Timer
	modifyingLock sync.RWMutex
	runningLock   sync.Mutex
	isPending     bool
}

func (peer *Peer) NewTimer(expirationFunction func(*Peer)) *Timer {
	timer := &Timer{}
	timer.Timer = time.AfterFunc(time.Hour, func() {
		timer.runningLock.Lock()

		timer.modifyingLock.Lock()
		if !timer.isPending {
			timer.modifyingLock.Unlock()
			timer.runningLock.Unlock()
			return
		}
		timer.isPending = false
		timer.modifyingLock.Unlock()

		expirationFunction(peer)
		timer.runningLock.Unlock()
	})
	timer.Stop()
	return timer
}

func (timer *Timer) Mod(d time.Duration) {
	timer.modifyingLock.Lock()
	timer.isPending = true
	timer.Reset(d)
	timer.modifyingLock.Unlock()
}

func (timer *Timer) Del() {
	timer.modifyingLock.Lock()
	timer.isPending = false
	timer.Stop()
	timer.modifyingLock.Unlock()
}

func (timer *Timer) DelSync() {
	timer.Del()
	timer.runningLock.Lock()
	timer.Del()
	timer.runningLock.Unlock()
}

func (timer *Timer) IsPending() bool {
	timer.modifyingLock.RLock()
	defer timer.modifyingLock.RUnlock()
	return timer.isPending
}

func (peer *Peer) timersActive() bool {
	if !peer.isRunning.Get() {
		return false
	}

	peer.RLock()
	device := peer.device
	peer.RUnlock()

	if device == nil || !device.isUp.Get() {
		return false
	}

	device.peers.RLock()
	defer device.peers.RUnlock()

	return len(device.peers.keyMap) > 0
}

func expiredRetransmitHandshake(peer *Peer) {
	if atomic.LoadUint32(&peer.timers.handshakeAttempts) > MaxTimerHandshakes {
		peer.device.log.Debug.Printf("%s - Handshake did not complete after %d attempts, giving up\n", peer, MaxTimerHandshakes+2)

		if peer.timersActive() {
			peer.timers.sendKeepalive.Del()
		}

		/* We drop all packets without a keypair and don't try again,
		 * if we try unsuccessfully for too long to make a handshake.
		 */
		peer.FlushNonceQueue()

		/* We set a timer for destroying any residue that might be left
		 * of a partial exchange.
		 */
		if peer.timersActive() && !peer.timers.zeroKeyMaterial.IsPending() {
			peer.timers.zeroKeyMaterial.Mod(RejectAfterTime * 3)
		}
	} else {
		atomic.AddUint32(&peer.timers.handshakeAttempts, 1)
		if false {
			peer.device.log.Debug.Printf("%s - Handshake did not complete after %d seconds, retrying (try %d)\n", peer, int(RekeyTimeout.Seconds()), atomic.LoadUint32(&peer.timers.handshakeAttempts)+1)
		}

		/* We clear the endpoint address src address, in case this is the cause of trouble. */
		peer.Lock()
		if peer.endpoint != nil {
			peer.endpoint.ClearSrc()
		}
		peer.Unlock()

		peer.SendHandshakeInitiation(true)
	}
}

func expiredSendKeepalive(peer *Peer) {
	peer.SendKeepalive()
	if peer.timers.needAnotherKeepalive.Get() {
		peer.timers.needAnotherKeepalive.Set(false)
		if peer.timersActive() {
			peer.timers.sendKeepalive.Mod(KeepaliveTimeout)
		}
	}
}

func expiredNewHandshake(peer *Peer) {
	peer.device.log.Debug.Printf("%s - Retrying handshake because we stopped hearing back after %d seconds\n", peer, int((KeepaliveTimeout + RekeyTimeout).Seconds()))
	/* We clear the endpoint address src address, in case this is the cause of trouble. */
	peer.Lock()
	if peer.endpoint != nil {
		peer.endpoint.ClearSrc()
	}
	peer.Unlock()
	peer.SendHandshakeInitiation(false)

}

func expiredZeroKeyMaterial(peer *Peer) {
	peer.device.log.Debug.Printf("%s - Removing all keys, since we haven't received a new one in %d seconds\n", peer, int((RejectAfterTime * 3).Seconds()))
	peer.ZeroAndFlushAll()
}

func expiredPersistentKeepalive(peer *Peer) {
	peer.RLock()
	persistentKeepaliveInterval := peer.persistentKeepaliveInterval
	peer.RUnlock()

	if persistentKeepaliveInterval > 0 {
		peer.SendKeepalive()
	}
}

/* Should be called after an authenticated data packet is sent. */
func (peer *Peer) timersDataSent() {
	if peer.timersActive() && !peer.timers.newHandshake.IsPending() {
		peer.timers.newHandshake.Mod(KeepaliveTimeout + RekeyTimeout + time.Millisecond*time.Duration(rand.Int31n(RekeyTimeoutJitterMaxMs)))
	}
}

/* Should be called after an authenticated data packet is received. */
func (peer *Peer) timersDataReceived() {
	if peer.timersActive() {
		if !peer.timers.sendKeepalive.IsPending() {
			peer.timers.sendKeepalive.Mod(KeepaliveTimeout)
		} else {
			peer.timers.needAnotherKeepalive.Set(true)
		}
	}
}

/* Should be called after any type of authenticated packet is sent -- keepalive, data, or handshake. */
func (peer *Peer) timersAnyAuthenticatedPacketSent() {
	if peer.timersActive() {
		peer.timers.sendKeepalive.Del()
	}
}

/* Should be called after any type of authenticated packet is received -- keepalive, data, or handshake. */
func (peer *Peer) timersAnyAuthenticatedPacketReceived() {
	if peer.timersActive() {
		peer.timers.newHandshake.Del()
	}
}

/* Should be called after a handshake initiation message is sent. */
func (peer *Peer) timersHandshakeInitiated() {
	if peer.timersActive() {
		timeout := RekeyTimeout
		attempts := atomic.LoadUint32(&peer.timers.handshakeAttempts)
		if attempts == 0 {
			attempts = 1
		}
		if t := time.Duration(attempts) * time.Second; t < timeout {
			timeout = t
		}
		peer.timers.retransmitHandshake.Mod(timeout + time.Millisecond*time.Duration(rand.Int31n(RekeyTimeoutJitterMaxMs)))
	}
}

/* Should be called after a handshake response message is received and processed or when getting key confirmation via the first data message. */
func (peer *Peer) timersHandshakeComplete() {
	if peer.timersActive() {
		peer.timers.retransmitHandshake.Del()
	}
	atomic.StoreUint32(&peer.timers.handshakeAttempts, 0)
	peer.timers.sentLastMinuteHandshake.Set(false)
	atomic.StoreInt64(&peer.stats.lastHandshakeNano, time.Now().UnixNano())
}

/* Should be called after an ephemeral key is created, which is before sending a handshake response or after receiving a handshake response. */
func (peer *Peer) timersSessionDerived() {
	if peer.timersActive() {
		peer.timers.zeroKeyMaterial.Mod(RejectAfterTime * 3)
	}
}

/* Should be called before a packet with authentication -- keepalive, data, or handshake -- is sent, or after one is received. */
func (peer *Peer) timersAnyAuthenticatedPacketTraversal() {
	if !peer.timersActive() {
		return
	}

	peer.RLock()
	persistentKeepaliveInterval := peer.persistentKeepaliveInterval
	peer.RUnlock()

	if persistentKeepaliveInterval > 0 {
		peer.timers.persistentKeepalive.Mod(time.Duration(persistentKeepaliveInterval) * time.Second)
	}
}

func (peer *Peer) timersInit() {
	peer.timers.retransmitHandshake = peer.NewTimer(expiredRetransmitHandshake)
	peer.timers.sendKeepalive = peer.NewTimer(expiredSendKeepalive)
	peer.timers.newHandshake = peer.NewTimer(expiredNewHandshake)
	peer.timers.zeroKeyMaterial = peer.NewTimer(expiredZeroKeyMaterial)
	peer.timers.persistentKeepalive = peer.NewTimer(expiredPersistentKeepalive)
	atomic.StoreUint32(&peer.timers.handshakeAttempts, 0)
	peer.timers.sentLastMinuteHandshake.Set(false)
	peer.timers.needAnotherKeepalive.Set(false)
}

func (peer *Peer) timersStop() {
	peer.timers.retransmitHandshake.DelSync()
	peer.timers.sendKeepalive.DelSync()
	peer.timers.newHandshake.DelSync()
	peer.timers.zeroKeyMaterial.DelSync()
	peer.timers.persistentKeepalive.DelSync()
}
