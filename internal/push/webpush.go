// Package push 实现 Web Push 投递：VAPID 配置、真实发送（Sender）、
// 以及面向中枢能力 notify.phone 的 CentralNotifier（见 central.go）。
package push

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"

	webpush "github.com/SherClockHolmes/webpush-go"

	"github.com/zzjcool/anotify/internal/store"
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
// 注意：webpush-go 会自动给非 https 的 subscriber 加 "mailto:" 前缀，
// 因此这里若 subject 已带 mailto: 要先去掉，避免 "mailto:mailto:" 双重前缀
// （Apple web.push 会返回 BadJwtToken）。
func (c *VAPIDConfig) options(ttl int, urgency webpush.Urgency) *webpush.Options {
	sub := c.Subscriber
	sub = strings.TrimPrefix(sub, "mailto:")
	return &webpush.Options{
		Subscriber:      sub,
		VAPIDPublicKey:  c.PublicKey,
		VAPIDPrivateKey: c.PrivateKey,
		TTL:             ttl,
		Urgency:         urgency,
	}
}

// 本文件是 #80 从 dispatcher.go 拆出来的：Dispatcher（旧消息总线消费者）
// 随 broker 包一起删除，但 Sender 这一层（真实 Web Push 发送）仍是
// notify.phone 的执行依赖，所以独立留下。
//
// 注意：本仓库不含 dispatcher.go。若 LSP 报「Sender redeclared」，
// 是分析与另一个 checkout（同一 module 路径）混在了一起。

// Sender 抽象一次 Web Push 发送，便于测试注入。
type Sender interface {
	Send(ctx context.Context, dev *store.Device, payload []byte, ttl int, urgency webpush.Urgency) (statusCode int, err error)
}

// SenderFunc 适配普通函数到 Sender。
type SenderFunc func(ctx context.Context, dev *store.Device, payload []byte, ttl int, urgency webpush.Urgency) (int, error)

// Send 实现 Sender。
func (f SenderFunc) Send(ctx context.Context, dev *store.Device, payload []byte, ttl int, urgency webpush.Urgency) (int, error) {
	return f(ctx, dev, payload, ttl, urgency)
}

// webpushSender 用 webpush-go + VAPID 真实发送。
type webpushSender struct {
	cfg *VAPIDConfig
}

// NewVAPIDSender 构造一个基于 VAPID 的真实 Sender。
func NewVAPIDSender(cfg *VAPIDConfig) Sender {
	return &webpushSender{cfg: cfg}
}

func (s *webpushSender) Send(ctx context.Context, dev *store.Device, payload []byte, ttl int, urgency webpush.Urgency) (int, error) {
	sub := &webpush.Subscription{
		Endpoint: dev.Endpoint,
		Keys: webpush.Keys{
			Auth:   dev.Auth,
			P256dh: dev.P256dh,
		},
	}
	resp, err := webpush.SendNotificationWithContext(ctx, payload, sub, s.cfg.options(ttl, urgency))
	if err != nil {
		return 0, fmt.Errorf("webpush send: %w", err)
	}
	defer resp.Body.Close()
	// 读取 body 以便连接复用 & 获取错误细节
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
	if resp.StatusCode >= 400 {
		return resp.StatusCode, fmt.Errorf("webpush status %d: %s", resp.StatusCode, string(body))
	}
	return resp.StatusCode, nil
}

// urgencyFor 按优先级映射 Web Push urgency。
func urgencyFor(priority string) webpush.Urgency {
	switch priority {
	case "high":
		return webpush.UrgencyHigh
	case "low":
		return webpush.UrgencyLow
	case "veryLow":
		return webpush.UrgencyVeryLow
	default:
		return webpush.UrgencyNormal
	}
}
