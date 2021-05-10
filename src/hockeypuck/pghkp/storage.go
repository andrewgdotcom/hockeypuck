/*
   Hockeypuck - OpenPGP key server
   Copyright (C) 2012-2014  Casey Marshall

   This program is free software: you can redistribute it and/or modify
   it under the terms of the GNU Affero General Public License as published by
   the Free Software Foundation, version 3.

   This program is distributed in the hope that it will be useful,
   but WITHOUT ANY WARRANTY; without even the implied warranty of
   MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
   GNU Affero General Public License for more details.

   You should have received a copy of the GNU Affero General Public License
   along with this program.  If not, see <http://www.gnu.org/licenses/>.
*/

package pghkp

import (
	"bytes"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	_ "github.com/lib/pq"
	"github.com/pkg/errors"

	"hockeypuck/hkp/jsonhkp"
	hkpstorage "hockeypuck/hkp/storage"
	log "hockeypuck/logrus"
	"hockeypuck/openpgp"
)

const (
	maxInsertErrors = 100
)

type storage struct {
	*sql.DB
	dbName  string
	options []openpgp.KeyReaderOption

	mu        sync.Mutex
	listeners []func(hkpstorage.KeyChange) error
}

var _ hkpstorage.Storage = (*storage)(nil)

var crTablesSQL = []string{
	`CREATE TABLE IF NOT EXISTS keys (
rfingerprint TEXT NOT NULL PRIMARY KEY,
doc jsonb NOT NULL,
ctime TIMESTAMP WITH TIME ZONE NOT NULL,
mtime TIMESTAMP WITH TIME ZONE NOT NULL,
md5 TEXT NOT NULL UNIQUE,
keywords tsvector
)`,
	`CREATE TABLE IF NOT EXISTS subkeys (
rfingerprint TEXT NOT NULL,
rsubfp TEXT NOT NULL PRIMARY KEY,
FOREIGN KEY (rfingerprint) REFERENCES keys(rfingerprint)
)
`,
}

var crIndexesSQL = []string{
	`CREATE INDEX IF NOT EXISTS keys_rfp ON keys(rfingerprint text_pattern_ops);`,
	`CREATE INDEX IF NOT EXISTS keys_ctime ON keys(ctime);`,
	`CREATE INDEX IF NOT EXISTS keys_mtime ON keys(mtime);`,
	`CREATE INDEX IF NOT EXISTS keys_keywords ON keys USING gin(keywords);`,
	`CREATE INDEX IF NOT EXISTS subkeys_rfp ON subkeys(rsubfp text_pattern_ops);`,
}

var drConstraintsSQL = []string{
	`ALTER TABLE keys DROP CONSTRAINT keys_pk;`,
	`ALTER TABLE keys DROP CONSTRAINT keys_md5;`,
	`DROP INDEX keys_rfp;`,
	`DROP INDEX keys_ctime;`,
	`DROP INDEX keys_mtime;`,
	`DROP INDEX keys_keywords;`,

	`ALTER TABLE subkeys DROP CONSTRAINT subkeys_pk;`,
	`ALTER TABLE subkeys DROP CONSTRAINT subkeys_fk;`,
	`DROP INDEX subkeys_rfp;`,
}

// Dial returns PostgreSQL storage connected to the given database URL.
func Dial(url string, options []openpgp.KeyReaderOption) (hkpstorage.Storage, error) {
	db, err := sql.Open("postgres", url)
	if err != nil {
		return nil, errors.WithStack(err)
	}
	return New(db, options)
}

// New returns a PostgreSQL storage implementation for an HKP service.
func New(db *sql.DB, options []openpgp.KeyReaderOption) (hkpstorage.Storage, error) {
	st := &storage{
		DB:      db,
		options: options,
	}
	err := st.createTables()
	if err != nil {
		return nil, errors.Wrap(err, "failed to create tables")
	}
	err = st.createIndexes()
	if err != nil {
		return nil, errors.Wrap(err, "failed to create indexes")
	}
	return st, nil
}

func (st *storage) createTables() error {
	for _, crTableSQL := range crTablesSQL {
		_, err := st.Exec(crTableSQL)
		if err != nil {
			return errors.WithStack(err)
		}
	}
	return nil
}

func (st *storage) createIndexes() error {
	for _, crIndexSQL := range crIndexesSQL {
		_, err := st.Exec(crIndexSQL)
		if err != nil {
			return errors.WithStack(err)
		}
	}
	return nil
}

type keyDoc struct {
	RFingerprint string
	CTime        time.Time
	MTime        time.Time
	MD5          string
	Doc          string
	Keywords     []string
}

func (st *storage) MatchMD5(md5s []string) ([]string, error) {
	var md5In []string
	for _, md5 := range md5s {
		// Must validate to prevent SQL injection since we're appending SQL strings here.
		_, err := hex.DecodeString(md5)
		if err != nil {
			return nil, errors.Wrapf(err, "invalid MD5 %q", md5)
		}
		md5In = append(md5In, "'"+strings.ToLower(md5)+"'")
	}

	sqlStr := fmt.Sprintf("SELECT rfingerprint FROM keys WHERE md5 IN (%s)", strings.Join(md5In, ","))
	rows, err := st.Query(sqlStr)
	if err != nil {
		return nil, errors.WithStack(err)
	}

	var result []string
	defer rows.Close()
	for rows.Next() {
		var rfp string
		err := rows.Scan(&rfp)
		if err != nil && err != sql.ErrNoRows {
			return nil, errors.WithStack(err)
		}
		result = append(result, rfp)
	}
	err = rows.Err()
	if err != nil {
		return nil, errors.WithStack(err)
	}
	return result, nil
}

// Resolve implements storage.Storage.
//
// Only v4 key IDs are resolved by this backend. v3 short and long key IDs
// currently won't match.
func (st *storage) Resolve(keyids []string) (_ []string, retErr error) {
	var result []string
	sqlStr := "SELECT rfingerprint FROM keys WHERE rfingerprint LIKE $1 || '%'"
	stmt, err := st.Prepare(sqlStr)
	if err != nil {
		return nil, errors.WithStack(err)
	}
	defer stmt.Close()

	var subKeyIDs []string
	for _, keyid := range keyids {
		keyid = strings.ToLower(keyid)
		var rfp string
		row := stmt.QueryRow(keyid)
		err = row.Scan(&rfp)
		if err == sql.ErrNoRows {
			subKeyIDs = append(subKeyIDs, keyid)
		} else if err != nil {
			return nil, errors.WithStack(err)
		}
		result = append(result, rfp)
	}

	if len(subKeyIDs) > 0 {
		subKeyResult, err := st.resolveSubKeys(subKeyIDs)
		if err != nil {
			return nil, errors.WithStack(err)
		}
		result = append(result, subKeyResult...)
	}

	return result, nil
}

func (st *storage) resolveSubKeys(keyids []string) ([]string, error) {
	var result []string
	sqlStr := "SELECT rfingerprint FROM subkeys WHERE rsubfp LIKE $1 || '%'"
	stmt, err := st.Prepare(sqlStr)
	if err != nil {
		return nil, errors.WithStack(err)
	}
	defer stmt.Close()

	for _, keyid := range keyids {
		keyid = strings.ToLower(keyid)
		var rfp string
		row := stmt.QueryRow(keyid)
		err = row.Scan(&rfp)
		if err != nil && err != sql.ErrNoRows {
			return nil, errors.WithStack(err)
		}
		result = append(result, rfp)
	}

	return result, nil
}

func (st *storage) MatchKeyword(search []string) ([]string, error) {
	var result []string
	stmt, err := st.Prepare("SELECT rfingerprint FROM keys WHERE keywords @@ plainto_tsquery($1) LIMIT $2")
	if err != nil {
		return nil, errors.WithStack(err)
	}
	defer stmt.Close()

	for _, term := range search {
		err = func() error {
			rows, err := stmt.Query(term, 100)
			if err != nil {
				return errors.WithStack(err)
			}
			defer rows.Close()
			for rows.Next() {
				var rfp string
				err = rows.Scan(&rfp)
				if err != nil && err != sql.ErrNoRows {
					return errors.WithStack(err)
				}
				result = append(result, rfp)
			}
			err = rows.Err()
			if err != nil {
				return errors.WithStack(err)
			}
			return nil
		}()
		if err != nil {
			return nil, err
		}
	}
	return result, nil
}

func (st *storage) ModifiedSince(t time.Time) ([]string, error) {
	var result []string
	rows, err := st.Query("SELECT rfingerprint FROM keys WHERE mtime > $1 ORDER BY mtime DESC LIMIT 100", t.UTC())
	if err != nil {
		return nil, errors.WithStack(err)
	}
	defer rows.Close()
	for rows.Next() {
		var rfp string
		err = rows.Scan(&rfp)
		if err != nil && err != sql.ErrNoRows {
			return nil, errors.WithStack(err)
		}
		result = append(result, rfp)
	}
	err = rows.Err()
	if err != nil {
		return nil, errors.WithStack(err)
	}
	return result, nil
}

func (st *storage) FetchKeys(rfps []string) ([]*openpgp.PrimaryKey, error) {
	if len(rfps) == 0 {
		return nil, nil
	}

	var rfpIn []string
	for _, rfp := range rfps {
		_, err := hex.DecodeString(rfp)
		if err != nil {
			return nil, errors.Wrapf(err, "invalid rfingerprint %q", rfp)
		}
		rfpIn = append(rfpIn, "'"+strings.ToLower(rfp)+"'")
	}
	sqlStr := fmt.Sprintf("SELECT doc FROM keys WHERE rfingerprint IN (%s)", strings.Join(rfpIn, ","))
	rows, err := st.Query(sqlStr)
	if err != nil {
		return nil, errors.WithStack(err)
	}

	var result []*openpgp.PrimaryKey
	for rows.Next() {
		var bufStr string
		err = rows.Scan(&bufStr)
		if err != nil && err != sql.ErrNoRows {
			return nil, errors.WithStack(err)
		}
		var pk jsonhkp.PrimaryKey
		err = json.Unmarshal([]byte(bufStr), &pk)
		if err != nil {
			return nil, errors.WithStack(err)
		}

		rfp := openpgp.Reverse(pk.Fingerprint)
		key, err := readOneKey(pk.Bytes(), rfp)
		if err != nil {
			return nil, errors.WithStack(err)
		}
		result = append(result, key)
	}
	err = rows.Err()
	if err != nil {
		return nil, errors.WithStack(err)
	}

	return result, nil
}

func (st *storage) FetchKeyrings(rfps []string) ([]*hkpstorage.Keyring, error) {
	var rfpIn []string
	for _, rfp := range rfps {
		_, err := hex.DecodeString(rfp)
		if err != nil {
			return nil, errors.Wrapf(err, "invalid rfingerprint %q", rfp)
		}
		rfpIn = append(rfpIn, "'"+strings.ToLower(rfp)+"'")
	}
	sqlStr := fmt.Sprintf("SELECT ctime, mtime, doc FROM keys WHERE rfingerprint IN (%s)", strings.Join(rfpIn, ","))
	rows, err := st.Query(sqlStr)
	if err != nil {
		return nil, errors.WithStack(err)
	}

	var result []*hkpstorage.Keyring
	for rows.Next() {
		var bufStr string
		var kr hkpstorage.Keyring
		err = rows.Scan(&bufStr, &kr.CTime, &kr.MTime)
		if err != nil && err != sql.ErrNoRows {
			return nil, errors.WithStack(err)
		}
		var pk jsonhkp.PrimaryKey
		err = json.Unmarshal([]byte(bufStr), &pk)
		if err != nil {
			return nil, errors.WithStack(err)
		}

		rfp := openpgp.Reverse(pk.Fingerprint)
		key, err := readOneKey(pk.Bytes(), rfp)
		if err != nil {
			return nil, errors.WithStack(err)
		}
		kr.PrimaryKey = key
		result = append(result, &kr)
	}
	err = rows.Err()
	if err != nil {
		return nil, errors.WithStack(err)
	}

	return result, nil
}

func readOneKey(b []byte, rfingerprint string) (*openpgp.PrimaryKey, error) {
	kr := openpgp.NewKeyReader(bytes.NewBuffer(b))
	keys, err := kr.Read()
	if err != nil {
		return nil, errors.WithStack(err)
	}
	if len(keys) == 0 {
		return nil, nil
	} else if len(keys) > 1 {
		return nil, errors.Errorf("multiple keys in keyring: %v, %v", keys[0].Fingerprint(), keys[1].Fingerprint())
	}
	if keys[0].RFingerprint != rfingerprint {
		return nil, errors.Errorf("RFingerprint mismatch: expected=%q got=%q",
			rfingerprint, keys[0].RFingerprint)
	}
	return keys[0], nil
}

func (st *storage) insertKey(key *openpgp.PrimaryKey) (isDuplicate bool, retErr error) {
	tx, err := st.Begin()
	if err != nil {
		return false, errors.WithStack(err)
	}
	defer func() {
		if retErr != nil {
			tx.Rollback()
		} else {
			tx.Commit()
		}
	}()
	return st.insertKeyTx(tx, key)
}

func (st *storage) insertKeyTx(tx *sql.Tx, key *openpgp.PrimaryKey) (isDuplicate bool, retErr error) {
	stmt, err := tx.Prepare("INSERT INTO keys (rfingerprint, ctime, mtime, md5, doc, keywords) " +
		"SELECT $1::TEXT, $2::TIMESTAMP, $3::TIMESTAMP, $4::TEXT, $5::JSONB, to_tsvector($6) " +
		"WHERE NOT EXISTS (SELECT 1 FROM keys WHERE rfingerprint = $1)")
	if err != nil {
		return false, errors.WithStack(err)
	}
	defer stmt.Close()

	subStmt, err := tx.Prepare("INSERT INTO subkeys (rfingerprint, rsubfp) " +
		"SELECT $1::TEXT, $2::TEXT WHERE NOT EXISTS (SELECT 1 FROM subkeys WHERE rsubfp = $2)")
	if err != nil {
		return false, errors.WithStack(err)
	}
	defer subStmt.Close()

	openpgp.Sort(key)

	now := time.Now().UTC()
	jsonKey := jsonhkp.NewPrimaryKey(key)
	jsonBuf, err := json.Marshal(jsonKey)
	if err != nil {
		return false, errors.Wrapf(err, "cannot serialize rfp=%q", key.RFingerprint)
	}

	jsonStr := string(jsonBuf)
	keywords := keywordsTSVector(key)
	result, err := stmt.Exec(&key.RFingerprint, &now, &now, &key.MD5, &jsonStr, &keywords)
	if err != nil {
		return false, errors.Wrapf(err, "cannot insert rfp=%q", key.RFingerprint)
	}

	var keysInserted int64
	if keysInserted, err = result.RowsAffected(); err != nil {
		// We arrive here if the DB driver doesn't support
		// RowsAffected, although lib/pq is known to support it.
		// If it doesn't, then something has gone badly awry!
		return false, errors.Wrapf(err, "rows affected not available when inserting rfp=%q", key.RFingerprint)
	}

	var rowsAffected int64
	for _, subKey := range key.SubKeys {
		result, err := subStmt.Exec(&key.RFingerprint, &subKey.RFingerprint)
		if err != nil {
			return false, errors.Wrapf(err, "cannot insert rsubfp=%q", subKey.RFingerprint)
		}
		if rowsAffected, err = result.RowsAffected(); err != nil {
			// See above.
			return false, errors.Wrapf(err, "rows affected not available when inserting rsubfp=%q", subKey.RFingerprint)
		}
		keysInserted += rowsAffected
	}

	return keysInserted == 0, nil
}

func (st *storage) Insert(keys []*openpgp.PrimaryKey) (n int, retErr error) {
	var result hkpstorage.InsertError
	for _, key := range keys {
		if count, max := len(result.Errors), maxInsertErrors; count > max {
			result.Errors = append(result.Errors, errors.Errorf("too many insert errors (%d > %d), bailing...", count, max))
			return n, result
		}

		if isDuplicate, err := st.insertKey(key); err != nil {
			result.Errors = append(result.Errors, err)
			continue
		} else if isDuplicate {
			result.Duplicates = append(result.Duplicates, key)
			continue
		}

		st.Notify(hkpstorage.KeyAdded{
			ID:     key.KeyID(),
			Digest: key.MD5,
		})
		n++
	}

	if len(result.Duplicates) > 0 || len(result.Errors) > 0 {
		return n, result
	}
	return n, nil
}

func (st *storage) Replace(key *openpgp.PrimaryKey) (_ string, retErr error) {
	tx, err := st.Begin()
	if err != nil {
		return "", errors.WithStack(err)
	}
	defer func() {
		if retErr != nil {
			tx.Rollback()
		} else {
			retErr = tx.Commit()
		}
	}()
	md5, err := st.deleteTx(tx, key.Fingerprint())
	if err != nil {
		return "", errors.WithStack(err)
	}
	_, err = st.insertKeyTx(tx, key)
	if err != nil {
		return "", errors.WithStack(err)
	}
	return md5, nil
}

func (st *storage) Delete(fp string) (_ string, retErr error) {
	tx, err := st.Begin()
	if err != nil {
		return "", errors.WithStack(err)
	}
	defer func() {
		if retErr != nil {
			tx.Rollback()
		} else {
			retErr = tx.Commit()
		}
	}()
	md5, err := st.deleteTx(tx, fp)
	if err != nil {
		return "", errors.WithStack(err)
	}
	return md5, nil
}

func (st *storage) deleteTx(tx *sql.Tx, fp string) (string, error) {
	rfp := openpgp.Reverse(fp)
	_, err := tx.Exec("DELETE FROM subkeys WHERE rfingerprint = $1", rfp)
	if err != nil {
		return "", errors.WithStack(err)
	}
	var md5 string
	err = tx.QueryRow("DELETE FROM keys WHERE rfingerprint = $1 RETURNING md5", rfp).Scan(&md5)
	if err != nil {
		if err == sql.ErrNoRows {
			return "", errors.WithStack(hkpstorage.ErrKeyNotFound)
		}
		return "", errors.WithStack(err)
	}
	return md5, nil
}

func (st *storage) Update(key *openpgp.PrimaryKey, lastID string, lastMD5 string) (retErr error) {
	tx, err := st.Begin()
	if err != nil {
		return errors.WithStack(err)
	}
	defer func() {
		if retErr != nil {
			tx.Rollback()
		} else {
			tx.Commit()
		}
	}()

	openpgp.Sort(key)

	now := time.Now().UTC()
	jsonKey := jsonhkp.NewPrimaryKey(key)
	jsonBuf, err := json.Marshal(jsonKey)
	if err != nil {
		return errors.Wrapf(err, "cannot serialize rfp=%q", key.RFingerprint)
	}
	keywords := keywordsTSVector(key)
	_, err = tx.Exec("UPDATE keys SET mtime = $1, md5 = $2, keywords = to_tsvector($3), doc = $4 "+
		"WHERE rfingerprint = $5",
		&now, &key.MD5, &keywords, jsonBuf, &key.RFingerprint)
	if err != nil {
		return errors.WithStack(err)
	}
	for _, subKey := range key.SubKeys {
		_, err := tx.Exec("INSERT INTO subkeys (rfingerprint, rsubfp) "+
			"SELECT $1::TEXT, $2::TEXT WHERE NOT EXISTS (SELECT 1 FROM subkeys WHERE rsubfp = $2)",
			&key.RFingerprint, &subKey.RFingerprint)
		if err != nil {
			return errors.WithStack(err)
		}
	}

	st.Notify(hkpstorage.KeyReplaced{
		OldID:     lastID,
		OldDigest: lastMD5,
		NewID:     key.KeyID(),
		NewDigest: key.MD5,
	})
	return nil
}

func keywordsTSVector(key *openpgp.PrimaryKey) string {
	keywords := keywordsFromKey(key)
	tsv, err := keywordsToTSVector(keywords)
	if err != nil {
		// In this case we've found a key that generated
		// an invalid tsvector - this is pretty much guaranteed
		// to be a bogus key, since having a valid key with
		// user IDs that exceed limits is highly unlikely.
		// In the future we should catch this earlier and
		// reject it as a bad key, but for now we just skip
		// storing keyword information.
		log.Warningf("keywords for rfp=%q exceeds limit, ignoring: %v", key.RFingerprint, err)
		return ""
	}
	return tsv
}

// keywordsToTSVector converts a slice of keywords to a
// PostgreSQL tsvector. If the resulting tsvector would
// be considered invalid by PostgreSQL an error is
// returned instead.
func keywordsToTSVector(keywords []string) (string, error) {
	const (
		lexemeLimit   = 2048            // 2KB for single lexeme
		tsvectorLimit = 1 * 1024 * 1024 // 1MB for lexemes + positions
	)
	for _, k := range keywords {
		if l := len([]byte(k)); l >= lexemeLimit {
			return "", fmt.Errorf("keyword exceeds limit (%d >= %d)", l, lexemeLimit)
		}
	}
	tsv := strings.Join(keywords, " & ")

	// Allow overhead of 8 bytes for position per keyword.
	if l := len([]byte(tsv)) + len(keywords)*8; l >= tsvectorLimit {
		return "", fmt.Errorf("keywords exceeds limit (%d >= %d)", l, tsvectorLimit)
	}
	return tsv, nil
}

// keywordsFromKey returns a slice of searchable tokens
// extracted from the UserID packets keywords string of
// the given key.
func keywordsFromKey(key *openpgp.PrimaryKey) []string {
	m := make(map[string]bool)
	for _, uid := range key.UserIDs {
		s := strings.ToLower(uid.Keywords)
		lbr, rbr := strings.Index(s, "<"), strings.LastIndex(s, ">")
		if lbr != -1 && rbr > lbr {
			email := s[lbr+1 : rbr]
			m[email] = true

			parts := strings.SplitN(email, "@", 2)
			if len(parts) > 1 {
				username, domain := parts[0], parts[1]
				m[username] = true
				m[domain] = true
			}
		}
		if lbr != -1 {
			fields := strings.FieldsFunc(s[:lbr], func(r rune) bool {
				if !utf8.ValidRune(r) {
					return true
				}
				if unicode.IsLetter(r) || unicode.IsNumber(r) || r == '-' {
					return false
				}
				return true
			})
			for _, field := range fields {
				m[field] = true
			}
		}
	}
	var result []string
	for k := range m {
		result = append(result, k)
	}
	return result
}

func subkeys(key *openpgp.PrimaryKey) []string {
	var result []string
	for _, subkey := range key.SubKeys {
		result = append(result, subkey.RFingerprint)
	}
	return result
}

func (st *storage) Subscribe(f func(hkpstorage.KeyChange) error) {
	st.mu.Lock()
	st.listeners = append(st.listeners, f)
	st.mu.Unlock()
}

func (st *storage) Notify(change hkpstorage.KeyChange) error {
	st.mu.Lock()
	defer st.mu.Unlock()
	log.Debugf("%v", change)
	for _, f := range st.listeners {
		// TODO: log error notifying listener?
		f(change)
	}
	return nil
}

func (st *storage) RenotifyAll() error {
	sqlStr := fmt.Sprintf("SELECT md5 FROM keys")
	rows, err := st.Query(sqlStr)
	if err != nil {
		return errors.WithStack(err)
	}

	defer rows.Close()
	for rows.Next() {
		var md5 string
		err := rows.Scan(&md5)
		if err != nil {
			if err == sql.ErrNoRows {
				return nil
			} else {
				return errors.WithStack(err)
			}
		}
		st.Notify(hkpstorage.KeyAdded{Digest: md5})
	}
	err = rows.Err()
	return errors.WithStack(err)
}
