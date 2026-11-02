package store

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"database/sql"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
)

// newCipher builds the AEAD that seals env vars. key must be 32 bytes.
func newCipher(key []byte) (cipher.AEAD, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("create cipher: %w", err)
	}
	return cipher.NewGCM(block)
}

// seal encrypts env vars. The project id is authenticated with them, so a
// sealed value only opens for the project it was written for.
func (s *Store) seal(projectID int64, env map[string]string) ([]byte, error) {
	plain, err := json.Marshal(env)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, s.aead.NonceSize(), s.aead.NonceSize()+len(plain)+s.aead.Overhead())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	return s.aead.Seal(nonce, nonce, plain, projectAAD(projectID)), nil
}

func (s *Store) open(projectID int64, sealed []byte) (map[string]string, error) {
	env := map[string]string{}
	if len(sealed) == 0 {
		return env, nil
	}
	if len(sealed) < s.aead.NonceSize() {
		return nil, errors.New("open env vars: value too short")
	}
	nonce, data := sealed[:s.aead.NonceSize()], sealed[s.aead.NonceSize():]
	plain, err := s.aead.Open(nil, nonce, data, projectAAD(projectID))
	if err != nil {
		return nil, fmt.Errorf("open env vars, is SHIPIT_MASTER_KEY the one they were written with? %w", err)
	}
	if err := json.Unmarshal(plain, &env); err != nil {
		return nil, fmt.Errorf("decode env vars: %w", err)
	}
	return env, nil
}

func projectAAD(projectID int64) []byte {
	return binary.BigEndian.AppendUint64(nil, uint64(projectID))
}

// Env returns the env vars the next release of a project will use.
func (s *Store) Env(ctx context.Context, projectID int64) (map[string]string, error) {
	var sealed []byte
	err := s.db.QueryRowContext(ctx, `SELECT env FROM projects WHERE id = ?`, projectID).Scan(&sealed)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return s.open(projectID, sealed)
}

// SetEnv replaces a project's env vars.
func (s *Store) SetEnv(ctx context.Context, projectID int64, env map[string]string) error {
	sealed, err := s.seal(projectID, env)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `UPDATE projects SET env = ? WHERE id = ?`, sealed, projectID)
	return err
}

// UpdateEnv applies changes to a project's env vars and returns the result. A
// nil value removes the variable.
func (s *Store) UpdateEnv(ctx context.Context, projectID int64, changes map[string]*string) (map[string]string, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()

	var sealed []byte
	err = tx.QueryRowContext(ctx, `SELECT env FROM projects WHERE id = ?`, projectID).Scan(&sealed)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	env, err := s.open(projectID, sealed)
	if err != nil {
		return nil, err
	}
	for k, v := range changes {
		if v == nil {
			delete(env, k)
			continue
		}
		env[k] = *v
	}
	if sealed, err = s.seal(projectID, env); err != nil {
		return nil, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE projects SET env = ? WHERE id = ?`, sealed, projectID); err != nil {
		return nil, err
	}
	return env, tx.Commit()
}
