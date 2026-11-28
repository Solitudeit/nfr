package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/go-sql-driver/mysql"
	"github.com/solitudeit/nfr/internal/config"
)

const dbConfigFile = "db.json"

// DBConfig describes a MySQL connection target.
type DBConfig struct {
	Host     string `json:"host"`
	Port     int    `json:"port"`
	Database string `json:"database"`
	Username string `json:"username"`
	Password string `json:"password,omitempty"`
}

// DBStatus reports the live storage engine state to the admin panel.
type DBStatus struct {
	Mode           string `json:"mode"` // "file" | "mysql"
	Configured     bool   `json:"configured"`
	FromConfig     bool   `json:"from_config"`
	MandatorySetup bool   `json:"mandatory_setup,omitempty"`
	Host           string `json:"host,omitempty"`
	Port           int    `json:"port,omitempty"`
	Database       string `json:"database,omitempty"`
	Username       string `json:"username,omitempty"`
	HasPassword    bool   `json:"has_password,omitempty"`
	Version        string `json:"version,omitempty"`
	Error          string `json:"error,omitempty"`
}

func dbConfigFromServer(d config.Database) DBConfig {
	return DBConfig{
		Host:     strings.TrimSpace(d.Host),
		Port:     d.Port,
		Database: strings.TrimSpace(d.Name),
		Username: strings.TrimSpace(d.User),
		Password: d.Password,
	}.Normalized()
}

// Normalized trims whitespace and applies the default MySQL port (3306).
func (c DBConfig) Normalized() DBConfig {
	c.Host = strings.TrimSpace(c.Host)
	c.Database = strings.TrimSpace(c.Database)
	c.Username = strings.TrimSpace(c.Username)
	if c.Port <= 0 {
		c.Port = 3306
	}
	return c
}

// Validate verifies required MySQL fields.
func (c DBConfig) Validate() error {
	c = c.Normalized()
	if c.Host == "" {
		return errors.New("请填写 MySQL 主机地址")
	}
	if c.Port < 1 || c.Port > 65535 {
		return errors.New("MySQL 端口必须在 1-65535 之间")
	}
	if c.Database == "" {
		return errors.New("请填写数据库名")
	}
	if c.Username == "" {
		return errors.New("请填写数据库用户名")
	}
	return nil
}

// DSN formats a go-sql-driver/mysql DSN with UTF-8, UTC parsing, and timeouts.
func (c DBConfig) DSN() string {
	c = c.Normalized()
	mc := mysql.NewConfig()
	mc.User = c.Username
	mc.Passwd = c.Password
	mc.Net = "tcp"
	mc.Addr = net.JoinHostPort(c.Host, strconv.Itoa(c.Port))
	mc.DBName = c.Database
	mc.ParseTime = true
	mc.Loc = time.UTC
	mc.Timeout = 5 * time.Second
	mc.ReadTimeout = 10 * time.Second
	mc.WriteTimeout = 10 * time.Second
	mc.AllowNativePasswords = true
	mc.Params = map[string]string{
		"charset": "utf8mb4",
	}
	return mc.FormatDSN()
}

func loadDBConfigFile(dir string, key []byte) (DBConfig, bool, error) {
	path := filepath.Join(dir, dbConfigFile)
	raw, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return DBConfig{}, false, nil
		}
		return DBConfig{}, false, err
	}
	var cfg DBConfig
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return DBConfig{}, false, fmt.Errorf("parse %s: %w", path, err)
	}
	if cfg.Password != "" {
		plain, err := decryptSecret(key, cfg.Password)
		if err != nil {
			return DBConfig{}, false, fmt.Errorf("decrypt db password: %w", err)
		}
		cfg.Password = plain
	}
	cfg = cfg.Normalized()
	if err := cfg.Validate(); err != nil {
		return DBConfig{}, false, fmt.Errorf("invalid %s: %w", path, err)
	}
	return cfg, true, nil
}

func saveDBConfigFile(dir string, key []byte, cfg DBConfig) error {
	cfg = cfg.Normalized()
	if err := cfg.Validate(); err != nil {
		return err
	}
	disk := cfg
	if disk.Password != "" {
		enc, err := encryptSecret(key, disk.Password)
		if err != nil {
			return err
		}
		disk.Password = enc
	}
	body, err := json.MarshalIndent(disk, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".db-*.json")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name)
	if err := os.Chmod(name, 0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(body); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(name, filepath.Join(dir, dbConfigFile))
}
