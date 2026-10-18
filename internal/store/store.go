// Package store 封装 bbolt，提供 apps / snapshot / auth / meta 四类数据的持久化。
package store

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/gitMFQ/gopm2/internal/types"
	bolt "go.etcd.io/bbolt"
)

var (
	bucketApps     = []byte("apps")     // name -> App{Spec,Status}
	bucketSnapshot = []byte("snapshot") // name -> Spec（gopm2 save 的快照）
	bucketAuth     = []byte("auth")     // password_hash / tokens
	bucketMeta     = []byte("meta")     // schema_version 等
)

// Store 是 bbolt 的薄封装，所有方法线程安全（bbolt 本身保证）。
type Store struct {
	db *bolt.DB
}

// Open 打开（必要时创建）数据目录下的 gopm2.db。
func Open(dataDir string) (*Store, error) {
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return nil, err
	}
	db, err := bolt.Open(filepath.Join(dataDir, "gopm2.db"), 0o600, &bolt.Options{Timeout: 3 * time.Second})
	if err != nil {
		return nil, fmt.Errorf("open bolt db: %w", err)
	}
	s := &Store{db: db}
	err = db.Update(func(tx *bolt.Tx) error {
		for _, b := range [][]byte{bucketApps, bucketSnapshot, bucketAuth, bucketMeta} {
			if _, err := tx.CreateBucketIfNotExists(b); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

// Close 关闭数据库。
func (s *Store) Close() error { return s.db.Close() }

// PutApp 保存或更新一个 App 记录。
func (s *Store) PutApp(a *types.App) error {
	data, err := json.Marshal(a)
	if err != nil {
		return err
	}
	return s.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket(bucketApps).Put([]byte(a.Spec.Name), data)
	})
}

// GetApp 按名字读取 App，不存在返回 nil, nil。
func (s *Store) GetApp(name string) (*types.App, error) {
	var app *types.App
	err := s.db.View(func(tx *bolt.Tx) error {
		v := tx.Bucket(bucketApps).Get([]byte(name))
		if v == nil {
			return nil
		}
		var a types.App
		if err := json.Unmarshal(v, &a); err != nil {
			return err
		}
		app = &a
		return nil
	})
	return app, err
}

// ListApps 返回全部 App，按名字排序（bbolt 按 key 序遍历）。
func (s *Store) ListApps() ([]*types.App, error) {
	var out []*types.App
	err := s.db.View(func(tx *bolt.Tx) error {
		return tx.Bucket(bucketApps).ForEach(func(k, v []byte) error {
			var a types.App
			if err := json.Unmarshal(v, &a); err != nil {
				return fmt.Errorf("decode app %q: %w", k, err)
			}
			out = append(out, &a)
			return nil
		})
	})
	return out, err
}

// DeleteApp 删除 App 记录。
func (s *Store) DeleteApp(name string) error {
	return s.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket(bucketApps).Delete([]byte(name))
	})
}

// SaveSnapshot 将当前 apps 列表固化为 snapshot（只保留 DesiredOn 的 Spec）。
func (s *Store) SaveSnapshot(apps []*types.App) error {
	return s.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket(bucketSnapshot)
		// bbolt 禁止在 ForEach(cursor) 迭代中删除当前 key（未定义行为），先收集再删
		var keys [][]byte
		if err := b.ForEach(func(k, _ []byte) error {
			keys = append(keys, append([]byte(nil), k...))
			return nil
		}); err != nil {
			return err
		}
		for _, k := range keys {
			if err := b.Delete(k); err != nil {
				return err
			}
		}
		for _, a := range apps {
			if !a.Status.DesiredOn {
				continue
			}
			data, err := json.Marshal(a.Spec)
			if err != nil {
				return err
			}
			if err := b.Put([]byte(a.Spec.Name), data); err != nil {
				return err
			}
		}
		return nil
	})
}

// LoadSnapshot 读取快照中的 Spec 列表。
func (s *Store) LoadSnapshot() ([]*types.Spec, error) {
	var out []*types.Spec
	err := s.db.View(func(tx *bolt.Tx) error {
		return tx.Bucket(bucketSnapshot).ForEach(func(k, v []byte) error {
			var spec types.Spec
			if err := json.Unmarshal(v, &spec); err != nil {
				return fmt.Errorf("decode snapshot %q: %w", k, err)
			}
			out = append(out, &spec)
			return nil
		})
	})
	return out, err
}

// SetAuth / GetAuth 操作 auth bucket 的键值。
func (s *Store) SetAuth(key string, value []byte) error {
	return s.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket(bucketAuth).Put([]byte(key), value)
	})
}

// GetAuth 读取 auth 键，不存在返回 nil, nil。
func (s *Store) GetAuth(key string) ([]byte, error) {
	var out []byte
	err := s.db.View(func(tx *bolt.Tx) error {
		v := tx.Bucket(bucketAuth).Get([]byte(key))
		if v != nil {
			out = append([]byte(nil), v...)
		}
		return nil
	})
	return out, err
}
