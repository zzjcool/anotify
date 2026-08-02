// Package push 实现 Web Push 派发器（Broker 的消费者2）。
package push

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"

	webpush "github.com/SherClockHolmes/webpush-go"
)

// VAPIDConfig 保存 VAPID 密钥对与订阅者（mailto）。
type VAPIDConfig struct {
	PublicKey  string `json:"publicKey"`
	PrivateKey string `json:"privateKey"`
	Subscriber string `json:"subscriber"` // 例 "mailto:notify@example.com"
}

// LoadVAPID 从环境变量或 JSON 文件加载 VAPID 配置。
//
// 优先级：
//  1. 环境变量 ANOTIFY_VAPID_PUBLIC_KEY / ANOTIFY_VAPID_PRIVATE_KEY / ANOTIFY_VAPID_SUBJECT
//  2. ANOTIFY_VAPID_FILE 指定的 JSON 文件（含 publicKey/privateKey 字段，兼容原型 vapid.json）
func LoadVAPID() (*VAPIDConfig, error) {
	pub := os.Getenv("ANOTIFY_VAPID_PUBLIC_KEY")
	priv := os.Getenv("ANOTIFY_VAPID_PRIVATE_KEY")
	sub := os.Getenv("ANOTIFY_VAPID_SUBJECT")

	if pub == "" || priv == "" {
		if f := os.Getenv("ANOTIFY_VAPID_FILE"); f != "" {
			cfg, err := loadVAPIDFile(f)
			if err != nil {
				return nil, err
			}
			if pub == "" {
				pub = cfg.PublicKey
			}
			if priv == "" {
				priv = cfg.PrivateKey
			}
			if sub == "" {
				sub = cfg.Subscriber
			}
		}
	}

	if pub == "" || priv == "" {
		return nil, fmt.Errorf("VAPID 未配置：请设置 ANOTIFY_VAPID_PUBLIC_KEY/ANOTIFY_VAPID_PRIVATE_KEY 或 ANOTIFY_VAPID_FILE")
	}
	if sub == "" {
		sub = "mailto:notify@anotify.dev"
	}
	if !strings.HasPrefix(sub, "mailto:") {
		sub = "mailto:" + sub
	}
	return &VAPIDConfig{PublicKey: pub, PrivateKey: priv, Subscriber: sub}, nil
}

func loadVAPIDFile(path string) (*VAPIDConfig, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read vapid file: %w", err)
	}
	var cfg VAPIDConfig
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return nil, fmt.Errorf("parse vapid file: %w", err)
	}
	return &cfg, nil
}

// options 把 VAPIDConfig 转成 webpush.Options（带 TTL 与 urgency）。
func (c *VAPIDConfig) options(ttl int, urgency webpush.Urgency) *webpush.Options {
	return &webpush.Options{
		Subscriber:      c.Subscriber,
		VAPIDPublicKey:  c.PublicKey,
		VAPIDPrivateKey: c.PrivateKey,
		TTL:             ttl,
		Urgency:         urgency,
	}
}
