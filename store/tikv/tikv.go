//
// tikv.go
// Copyright (C) 2018 YanMing <yming0221@gmail.com>
//
// Distributed under terms of the MIT license.
//

package tikv

import (
	"fmt"
	"math"
	"math/rand"
	"time"

	"github.com/pingcap/tidb/kv"
	ti "github.com/pingcap/tidb/store/tikv"
	"github.com/yongman/go/log"
	"github.com/yongman/tidis/config"
	"github.com/yongman/tidis/terror"
	"golang.org/x/net/context"
)

type Tikv struct {
	store    kv.Storage
	txnRetry int
}

func Open(conf *config.Config) (*Tikv, error) {
	d := ti.Driver{}
	store, err := d.Open(fmt.Sprintf("tikv://%s/pd?cluster=1", conf.PdAddr))
	if err != nil {
		return nil, err
	}
	return &Tikv{store: store, txnRetry: conf.TxnRetry}, nil
}

var (
	// retryBackOffBase is the initial duration, in microsecond, a failed transaction stays dormancy before it retries
	retryBackOffBase = 1
	// retryBackOffCap is the max amount of duration, in microsecond, a failed transaction stays dormancy before it retries
	retryBackOffCap = 100
)

func BackOff(attempts uint) int {
	upper := int(math.Min(float64(retryBackOffCap), float64(retryBackOffBase)*math.Pow(2.0, float64(attempts))))
	sleep := time.Duration(rand.Intn(upper)) * time.Millisecond
	time.Sleep(sleep)
	return int(sleep)
}

func (tikv *Tikv) GetTxnRetry() int {
	return tikv.txnRetry
}

func (tikv *Tikv) SetTxnRetry(count int) {
	tikv.txnRetry = count
}

func (tikv *Tikv) Close() error {
	return tikv.store.Close()
}

func (tikv *Tikv) Get(key []byte) ([]byte, error) {
	ss, err := tikv.store.GetSnapshot(kv.MaxVersion)
	if err != nil {
		return nil, err
	}
	v, err := ss.Get(key)
	if err != nil {
		if kv.IsErrNotFound(err) {
			return nil, nil
		}
	}
	return v, err
}

func (tikv *Tikv) GetWithSnapshot(key []byte, ss interface{}) ([]byte, error) {
	snapshot, ok := ss.(kv.Snapshot)
	if !ok {
		return nil, terror.ErrBackendType
	}
	v, err := snapshot.Get(key)
	if err != nil {
		if kv.IsErrNotFound(err) {
			return nil, nil
		}
	}
	return v, err
}

func (tikv *Tikv) GetNewestSnapshot() (interface{}, error) {
	return tikv.store.GetSnapshot(kv.MaxVersion)
}

func (tikv *Tikv) GetWithVersion(key []byte, version uint64) ([]byte, error) {
	ss, err := tikv.store.GetSnapshot(kv.Version{Ver: version})
	if err != nil {
		return nil, err
	}
	v, err := ss.Get(key)
	if err != nil {
		if kv.IsErrNotFound(err) {
			return nil, nil
		}
	}
	return v, err
}

func (tikv *Tikv) MGet(keys [][]byte) (map[string][]byte, error) {
	ss, err := tikv.store.GetSnapshot(kv.MaxVersion)
	if err != nil {
		return nil, err
	}
	// TODO
	nkeys := make([]kv.Key, len(keys))
	for i := 0; i < len(keys); i++ {
		nkeys[i] = keys[i]
	}
	return ss.BatchGet(nkeys)
}

func (tikv *Tikv) MGetWithVersion(keys [][]byte, version uint64) (map[string][]byte, error) {
	ss, err := tikv.store.GetSnapshot(kv.Version{Ver: version})
	if err != nil {
		return nil, err
	}
	// TODO
	nkeys := make([]kv.Key, len(keys))
	for i := 0; i < len(keys); i++ {
		nkeys[i] = keys[i]
	}
	return ss.BatchGet(nkeys)
}

func (tikv *Tikv) MGetWithSnapshot(keys [][]byte, ss interface{}) (map[string][]byte, error) {
	snapshot, ok := ss.(kv.Snapshot)
	if !ok {
		return nil, terror.ErrBackendType
	}
	// TODO
	nkeys := make([]kv.Key, len(keys))
	for i := 0; i < len(keys); i++ {
		nkeys[i] = keys[i]
	}
	return snapshot.BatchGet(nkeys)
}

// set must be run in txn
func (tikv *Tikv) Set(key []byte, value []byte) error {
	f := func(txn1 interface{}) (interface{}, error) {
		txn, _ := txn1.(kv.Transaction)
		err := txn.Set(key, value)
		return nil, err
	}

	_, err := tikv.BatchInTxn(f)
	return err
}

// map key cannot be []byte, use string
func (tikv *Tikv) MSet(kvm map[string][]byte) (int, error) {
	f := func(txn1 interface{}) (interface{}, error) {
		txn, _ := txn1.(kv.Transaction)

		for k, v := range kvm {
			err := txn.Set([]byte(k), v)
			if err != nil {
				return 0, err
			}
		}
		return len(kvm), nil
	}

	v, err := tikv.BatchInTxn(f)
	return v.(int), err
}

func (tikv *Tikv) Delete(keys [][]byte) (int, error) {
	f := func(txn1 interface{}) (interface{}, error) {
		txn, _ := txn1.(kv.Transaction)
		ss := txn.GetSnapshot()

		var deleted int = 0

		for _, k := range keys {
			v, _ := tikv.GetWithSnapshot(k, ss)
			if v != nil {
				deleted++
			}
			err := txn.Delete(k)
			if err != nil {
				return 0, err
			}
		}
		return deleted, nil
	}

	v, err := tikv.BatchInTxn(f)

	return v.(int), err
}

func (tikv *Tikv) getRangeKeysWithFrontier(start []byte, withstart bool, end []byte, withend bool, offset, limit uint64, snapshot interface{}, countOnly bool) ([][]byte, uint64, error) {
	// get latest ss
	var ss kv.Snapshot
	var err error
	var ok bool
	var count uint64 = 0
	if snapshot == nil {
		ss, err = tikv.store.GetSnapshot(kv.MaxVersion)
		if err != nil {
			return nil, 0, err
		}
	} else {
		ss, ok = snapshot.(kv.Snapshot)
		if !ok {
			return nil, 0, terror.ErrBackendType
		}
	}

	iter, err := ss.Seek(start)
	if err != nil {
		return nil, 0, err
	}
	defer iter.Close()

	var keys [][]byte

	for limit > 0 {
		if !iter.Valid() {
			break
		}

		key := iter.Key()

		err = iter.Next()
		if err != nil {
			return nil, 0, err
		}

		if !withstart && key.Cmp(start) == 0 {
			continue
		}
		if !withend && key.Cmp(end) == 0 {
			break
		}

		if end != nil && key.Cmp(end) > 0 {
			break
		}

		if offset > 0 {
			offset--
			continue
		}
		if countOnly {
			count++
		} else {
			keys = append(keys, key)
		}
		limit--
	}
	return keys, count, nil
}

func (tikv *Tikv) GetRangeKeysWithFrontier(start []byte, withstart bool, end []byte, withend bool, offset, limit uint64, snapshot interface{}) ([][]byte, error) {
	keys, _, err := tikv.getRangeKeysWithFrontier(start, withstart, end, withend, offset, limit, snapshot, false)
	return keys, err
}

func (tikv *Tikv) GetRangeKeysCount(start []byte, withstart bool, end []byte, withend bool, limit uint64, snapshot interface{}) (uint64, error) {
	_, cnt, err := tikv.getRangeKeysWithFrontier(start, withstart, end, withend, 0, limit, snapshot, true)
	return cnt, err
}

func (tikv *Tikv) GetRangeKeys(start []byte, end []byte, offset, limit uint64, snapshot interface{}) ([][]byte, error) {
	return tikv.GetRangeKeysWithFrontier(start, true, end, true, offset, limit, snapshot)
}

func (tikv *Tikv) GetRangeVals(start []byte, end []byte, limit uint64, snapshot interface{}) ([][]byte, error) {
	// get latest ss
	var ss kv.Snapshot
	var err error
	var ok bool
	if snapshot == nil {
		ss, err = tikv.store.GetSnapshot(kv.MaxVersion)
		if err != nil {
			return nil, err
		}
	} else {
		ss, ok = snapshot.(kv.Snapshot)
		if !ok {
			return nil, terror.ErrBackendType
		}
	}

	iter, err := ss.Seek(start)
	if err != nil {
		return nil, err
	}
	defer iter.Close()

	var vals [][]byte

	for limit > 0 {
		if !iter.Valid() {
			break
		}

		key := iter.Key()
		val := iter.Value()

		if end != nil && key.Cmp(end) > 0 {
			break
		}
		vals = append(vals, val)
		limit--
		err = iter.Next()
		if err != nil {
			return nil, err
		}
	}
	return vals, nil
}

func (tikv *Tikv) GetRangeKeysVals(start []byte, end []byte, limit uint64, snapshot interface{}) ([][]byte, error) {
	// get latest ss
	var ss kv.Snapshot
	var err error
	var ok bool
	if snapshot == nil {
		ss, err = tikv.store.GetSnapshot(kv.MaxVersion)
		if err != nil {
			return nil, err
		}
	} else {
		ss, ok = snapshot.(kv.Snapshot)
		if !ok {
			return nil, terror.ErrBackendType
		}
	}

	iter, err := ss.Seek(start)
	if err != nil {
		return nil, err
	}
	defer iter.Close()

	var keyvals [][]byte

	for limit > 0 {
		if !iter.Valid() {
			break
		}

		key := iter.Key()
		value := iter.Value()

		if end != nil && key.Cmp(end) > 0 {
			break
		}

		keyvals = append(keyvals, key)
		keyvals = append(keyvals, value)

		limit--
		err = iter.Next()
		if err != nil {
			return nil, err
		}
	}
	return keyvals, nil
}

func (tikv *Tikv) DeleteRange(start []byte, end []byte, limit uint64) (uint64, error) {
	// run in txn
	f := func(txn1 interface{}) (interface{}, error) {
		txn, _ := txn1.(kv.Transaction)

		ss := txn.GetSnapshot()

		iter, err := ss.Seek(start)
		if err != nil {
			return nil, err
		}
		defer iter.Close()

		var deleted uint64 = 0
		// limit == 0 means no limited
		if limit == 0 {
			limit = math.MaxUint64
		}

		for limit > 0 {
			if !iter.Valid() {
				break
			}

			key := iter.Key()

			if end != nil && key.Cmp(end) > 0 {
				break
			}
			err = txn.Delete(key)
			if err != nil {
				return nil, err
			}

			deleted++
			limit--

			err = iter.Next()
			if err != nil {
				return 0, err
			}
		}
		return deleted, nil
	}

	v, err := tikv.BatchInTxn(f)
	if err != nil {
		return 0, err
	}
	return v.(uint64), nil
}

func (tikv *Tikv) DeleteRangeWithTxn(start []byte, end []byte, limit uint64, txn1 interface{}) (uint64, error) {
	// run inside txn
	txn, ok := txn1.(kv.Transaction)
	if !ok {
		return 0, terror.ErrBackendType
	}
	ss := txn.GetSnapshot()

	iter, err := ss.Seek(start)
	if err != nil {
		return 0, err
	}
	defer iter.Close()

	var deleted uint64 = 0

	// limit == 0 means no limited
	if limit == 0 {
		limit = math.MaxUint64
	}
	for limit > 0 {
		if !iter.Valid() {
			break
		}

		key := iter.Key()

		if end != nil && key.Cmp(end) > 0 {
			break
		}
		err = txn.Delete(key)
		if err != nil {
			return 0, err
		}

		deleted++
		limit--

		err = iter.Next()
		if err != nil {
			return 0, err
		}
	}
	return deleted, nil

}
func (tikv *Tikv) BatchInTxn(f func(txn interface{}) (interface{}, error)) (interface{}, error) {
	var (
		retryCount int
		res        interface{}
		err        error
	)

	retryCount = tikv.GetTxnRetry()
	for retryCount >= 0 {
		txn, err := tikv.store.Begin()
		if err != nil {
			return nil, err
		}

		res, err = f(txn)
		if err != nil {
			err1 := txn.Rollback()
			if err1 != nil {
				if retryCount >= 0 && kv.IsRetryableError(err1) {
					log.Warnf("txn %v rollback retry, err: %v", txn, err1)
					retryCount--
					continue
				}
			}
			return nil, err1
		}
		err = txn.Commit(context.Background())
		if err == nil {
			break
		}
		if retryCount >= 0 && kv.IsRetryableError(err) {
			log.Warnf("txn %v commit retry, err: %v", txn, err)
			retryCount--
			continue
		}
	}
	return res, err
}
