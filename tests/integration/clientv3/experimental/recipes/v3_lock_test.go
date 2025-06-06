// Copyright 2016 The etcd Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package recipes_test

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"go.etcd.io/etcd/api/v3/mvccpb"
	clientv3 "go.etcd.io/etcd/client/v3"
	"go.etcd.io/etcd/client/v3/concurrency"
	recipe "go.etcd.io/etcd/client/v3/experimental/recipes"
	integration2 "go.etcd.io/etcd/tests/v3/framework/integration"
)

func TestMutexLockSingleNode(t *testing.T) {
	integration2.BeforeTest(t)

	clus := integration2.NewCluster(t, &integration2.ClusterConfig{Size: 3})
	defer clus.Terminate(t)

	var clients []*clientv3.Client
	testMutexLock(t, 5, integration2.MakeSingleNodeClients(t, clus, &clients))
	integration2.CloseClients(t, clients)
}

func TestMutexLockMultiNode(t *testing.T) {
	integration2.BeforeTest(t)

	clus := integration2.NewCluster(t, &integration2.ClusterConfig{Size: 3})
	defer clus.Terminate(t)

	var clients []*clientv3.Client
	testMutexLock(t, 5, integration2.MakeMultiNodeClients(t, clus, &clients))
	integration2.CloseClients(t, clients)
}

func testMutexLock(t *testing.T, waiters int, chooseClient func() *clientv3.Client) {
	// stream lock acquisitions
	lockedC := make(chan *concurrency.Mutex, waiters)
	errC := make(chan error, waiters)

	var wg sync.WaitGroup
	wg.Add(waiters)

	for i := 0; i < waiters; i++ {
		go func(i int) {
			defer wg.Done()
			session, err := concurrency.NewSession(chooseClient())
			if err != nil {
				errC <- fmt.Errorf("#%d: failed to create new session: %w", i, err)
				return
			}
			m := concurrency.NewMutex(session, "test-mutex")
			if err := m.Lock(t.Context()); err != nil {
				errC <- fmt.Errorf("#%d: failed to wait on lock: %w", i, err)
				return
			}
			lockedC <- m
		}(i)
	}
	// unlock locked mutexes
	timerC := time.After(time.Duration(waiters) * time.Second)
	for i := 0; i < waiters; i++ {
		select {
		case <-timerC:
			t.Fatalf("timed out waiting for lock %d", i)
		case err := <-errC:
			t.Fatalf("Unexpected error: %v", err)
		case m := <-lockedC:
			// lock acquired with m
			select {
			case <-lockedC:
				t.Fatalf("lock %d followers did not wait", i)
			default:
			}
			require.NoErrorf(t, m.Unlock(t.Context()), "could not release lock")
		}
	}
	wg.Wait()
}

func TestMutexTryLockSingleNode(t *testing.T) {
	integration2.BeforeTest(t)
	clus := integration2.NewCluster(t, &integration2.ClusterConfig{Size: 3})
	defer clus.Terminate(t)
	t.Logf("3 nodes cluster created...")
	var clients []*clientv3.Client
	testMutexTryLock(t, 5, integration2.MakeSingleNodeClients(t, clus, &clients))
	integration2.CloseClients(t, clients)
}

func TestMutexTryLockMultiNode(t *testing.T) {
	integration2.BeforeTest(t)
	clus := integration2.NewCluster(t, &integration2.ClusterConfig{Size: 3})
	defer clus.Terminate(t)

	var clients []*clientv3.Client
	testMutexTryLock(t, 5, integration2.MakeMultiNodeClients(t, clus, &clients))
	integration2.CloseClients(t, clients)
}

func testMutexTryLock(t *testing.T, lockers int, chooseClient func() *clientv3.Client) {
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()

	lockedC := make(chan *concurrency.Mutex)
	notlockedC := make(chan *concurrency.Mutex)

	for i := 0; i < lockers; i++ {
		go func(i int) {
			session, err := concurrency.NewSession(chooseClient())
			if err != nil {
				t.Error(err)
			}
			m := concurrency.NewMutex(session, "test-mutex-try-lock")
			err = m.TryLock(ctx)
			if err == nil {
				select {
				case lockedC <- m:
				case <-ctx.Done():
					t.Errorf("Thread: %v, Context failed: %v", i, err)
				}
			} else if errors.Is(err, concurrency.ErrLocked) {
				select {
				case notlockedC <- m:
				case <-ctx.Done():
					t.Errorf("Thread: %v, Context failed: %v", i, err)
				}
			} else {
				t.Errorf("Thread: %v; Unexpected Error %v", i, err)
			}
		}(i)
	}

	timerC := time.After(30 * time.Second)
	select {
	case <-lockedC:
		for i := 0; i < lockers-1; i++ {
			select {
			case <-lockedC:
				t.Fatalf("Multiple Mutes locked on same key")
			case <-notlockedC:
			case <-timerC:
				t.Errorf("timed out waiting for lock")
			}
		}
	case <-timerC:
		t.Errorf("timed out waiting for lock (30s)")
	}
}

// TestMutexSessionRelock ensures that acquiring the same lock with the same
// session will not result in deadlock.
func TestMutexSessionRelock(t *testing.T) {
	integration2.BeforeTest(t)

	clus := integration2.NewCluster(t, &integration2.ClusterConfig{Size: 3})
	defer clus.Terminate(t)
	session, err := concurrency.NewSession(clus.RandClient())
	if err != nil {
		t.Error(err)
	}

	m := concurrency.NewMutex(session, "test-mutex")
	require.NoError(t, m.Lock(t.Context()))

	m2 := concurrency.NewMutex(session, "test-mutex")
	require.NoError(t, m2.Lock(t.Context()))
}

// TestMutexWaitsOnCurrentHolder ensures a mutex is only acquired once all
// waiters older than the new owner are gone by testing the case where
// the waiter prior to the acquirer expires before the current holder.
func TestMutexWaitsOnCurrentHolder(t *testing.T) {
	integration2.BeforeTest(t)

	clus := integration2.NewCluster(t, &integration2.ClusterConfig{Size: 1})
	defer clus.Terminate(t)

	cctx := t.Context()

	cli := clus.Client(0)

	firstOwnerSession, err := concurrency.NewSession(cli)
	if err != nil {
		t.Error(err)
	}
	defer firstOwnerSession.Close()
	firstOwnerMutex := concurrency.NewMutex(firstOwnerSession, "test-mutex")
	require.NoError(t, firstOwnerMutex.Lock(cctx))

	victimSession, err := concurrency.NewSession(cli)
	if err != nil {
		t.Error(err)
	}
	defer victimSession.Close()
	victimDonec := make(chan struct{})
	go func() {
		defer close(victimDonec)
		concurrency.NewMutex(victimSession, "test-mutex").Lock(cctx)
	}()

	// ensure mutexes associated with firstOwnerSession and victimSession waits before new owner
	wch := cli.Watch(cctx, "test-mutex", clientv3.WithPrefix(), clientv3.WithRev(1))
	putCounts := 0
	for putCounts < 2 {
		select {
		case wrp := <-wch:
			putCounts += len(wrp.Events)
		case <-time.After(time.Second):
			t.Fatal("failed to receive watch response")
		}
	}
	require.Equalf(t, 2, putCounts, "expect 2 put events, but got %v", putCounts)

	newOwnerSession, err := concurrency.NewSession(cli)
	if err != nil {
		t.Error(err)
	}
	defer newOwnerSession.Close()
	newOwnerDonec := make(chan struct{})
	go func() {
		defer close(newOwnerDonec)
		concurrency.NewMutex(newOwnerSession, "test-mutex").Lock(cctx)
	}()

	select {
	case wrp := <-wch:
		require.Lenf(t, wrp.Events, 1, "expect a event, but got %v events", len(wrp.Events))
		e := wrp.Events[0]
		require.Equalf(t, mvccpb.PUT, e.Type, "expect a put event on prefix test-mutex, but got event type %v", e.Type)
	case <-time.After(time.Second):
		t.Fatalf("failed to receive a watch response")
	}

	// simulate losing the client that's next in line to acquire the lock
	victimSession.Close()

	// ensures the deletion of victim waiter from server side.
	select {
	case wrp := <-wch:
		require.Lenf(t, wrp.Events, 1, "expect a event, but got %v events", len(wrp.Events))
		e := wrp.Events[0]
		require.Equalf(t, mvccpb.DELETE, e.Type, "expect a delete event on prefix test-mutex, but got event type %v", e.Type)
	case <-time.After(time.Second):
		t.Fatal("failed to receive a watch response")
	}

	select {
	case <-newOwnerDonec:
		t.Fatal("new owner obtained lock before first owner unlocked")
	default:
	}

	require.NoError(t, firstOwnerMutex.Unlock(cctx))

	select {
	case <-newOwnerDonec:
	case <-time.After(time.Second):
		t.Fatal("new owner failed to obtain lock")
	}

	select {
	case <-victimDonec:
	case <-time.After(time.Second):
		t.Fatal("victim mutex failed to exit after first owner releases lock")
	}
}

func BenchmarkMutex4Waiters(b *testing.B) {
	integration2.BeforeTest(b)
	// XXX switch tests to use TB interface
	clus := integration2.NewCluster(nil, &integration2.ClusterConfig{Size: 3})
	defer clus.Terminate(nil)
	for i := 0; i < b.N; i++ {
		testMutexLock(nil, 4, func() *clientv3.Client { return clus.RandClient() })
	}
}

func TestRWMutexSingleNode(t *testing.T) {
	integration2.BeforeTest(t)
	clus := integration2.NewCluster(t, &integration2.ClusterConfig{Size: 3})
	defer clus.Terminate(t)
	testRWMutex(t, 5, func() *clientv3.Client { return clus.Client(0) })
}

func TestRWMutexMultiNode(t *testing.T) {
	integration2.BeforeTest(t)
	clus := integration2.NewCluster(t, &integration2.ClusterConfig{Size: 3})
	defer clus.Terminate(t)
	testRWMutex(t, 5, func() *clientv3.Client { return clus.RandClient() })
}

func testRWMutex(t *testing.T, waiters int, chooseClient func() *clientv3.Client) {
	// stream rwlock acquistions
	rlockedC := make(chan *recipe.RWMutex, 1)
	wlockedC := make(chan *recipe.RWMutex, 1)
	for i := 0; i < waiters; i++ {
		go func() {
			session, err := concurrency.NewSession(chooseClient())
			if err != nil {
				t.Error(err)
			}
			rwm := recipe.NewRWMutex(session, "test-rwmutex")
			if rand.Intn(2) == 0 {
				if err := rwm.RLock(); err != nil {
					t.Errorf("could not rlock (%v)", err)
				}
				rlockedC <- rwm
			} else {
				if err := rwm.Lock(); err != nil {
					t.Errorf("could not lock (%v)", err)
				}
				wlockedC <- rwm
			}
		}()
	}
	// unlock locked rwmutexes
	timerC := time.After(time.Duration(waiters) * time.Second)
	for i := 0; i < waiters; i++ {
		select {
		case <-timerC:
			t.Fatalf("timed out waiting for lock %d", i)
		case wl := <-wlockedC:
			select {
			case <-rlockedC:
				t.Fatalf("rlock %d readers did not wait", i)
			default:
			}
			require.NoErrorf(t, wl.Unlock(), "could not release lock")
		case rl := <-rlockedC:
			select {
			case <-wlockedC:
				t.Fatalf("rlock %d writers did not wait", i)
			default:
			}
			require.NoErrorf(t, rl.RUnlock(), "could not release rlock")
		}
	}
}
