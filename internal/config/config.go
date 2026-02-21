package config

import (
	"os"
	"path/filepath"
	"strconv"
)

type Config struct {
	Bind       string
	Port       int
	DBPath     string
	OpenAIKey  string
	GroqKey    string
	VAPIDEmail string
	EncryptKey string
}

func defaultDBPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return "openpoet.db"
	}
	dir := filepath.Join(home, ".openpoet")
	os.MkdirAll(dir, 0755)
	return filepath.Join(dir, "openpoet.db")
}

func Load() *Config {
	cfg := &Config{
		Bind:       getEnv("OPENPOET_BIND", "localhost"),
		Port:       getEnvInt("OPENPOET_PORT", 8080),
		DBPath:     getEnv("OPENPOET_DB", defaultDBPath()),
		OpenAIKey:  getEnv("OPENAI_API_KEY", ""),
		GroqKey:    getEnv("GROQ_API_KEY", ""),
		VAPIDEmail: getEnv("VAPID_EMAIL", "admin@openpoet.minhapalavra.com.br"),
		EncryptKey: getEnv("OPENPOET_ENCRYPT_KEY", ""),
	}
	return cfg
}

func getEnv(key, defaultVal string) string {
	if val := os.Getenv(key); val != "" {
		return val
	}
	return defaultVal
}

func getEnvInt(key string, defaultVal int) int {
	if val := os.Getenv(key); val != "" {
		if i, err := strconv.Atoi(val); err == nil {
			return i
		}
	}
	return defaultVal
}

func (c *Config) Address() string {
	return c.Bind + ":" + strconv.Itoa(c.Port)
}
