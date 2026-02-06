package config

import (
	"os"
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

func Load() *Config {
	cfg := &Config{
		Bind:       getEnv("DEVMANAGER_BIND", "0.0.0.0"),
		Port:       getEnvInt("DEVMANAGER_PORT", 8080),
		DBPath:     getEnv("DEVMANAGER_DB", "devmanager.db"),
		OpenAIKey:  getEnv("OPENAI_API_KEY", ""),
		GroqKey:    getEnv("GROQ_API_KEY", ""),
		VAPIDEmail: getEnv("VAPID_EMAIL", "admin@localhost"),
		EncryptKey: getEnv("DEVMANAGER_ENCRYPT_KEY", ""),
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
