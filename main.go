package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math/rand"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/caarlos0/env/v6"
	"github.com/gorilla/websocket"
)

type Config struct {
	HTTPPort  string `env:"HTTP_PORT" envDefault:"8088"`
	QQWSURL   string `env:"QQ_WS_URL" envDefault:"ws://127.0.0.1:3009"`
	QQGroupID int64  `env:"QQ_GROUP_ID"`
}

type ConnectionManager struct {
	conn     *websocket.Conn
	url      string
	mu       sync.RWMutex
	retries  int
	maxRetry int
	quit     chan struct{}
}

type OneBotMessage struct {
	PostType    string      `json:"post_type"`
	MessageType string      `json:"message_type"`
	Message     interface{} `json:"message"`
	UserID      int64       `json:"user_id"`
	GroupID     int64       `json:"group_id"`
}

type ResponseMessage struct {
	Action string      `json:"action"`
	Params interface{} `json:"params"`
}

var (
	upgrader = websocket.Upgrader{
		CheckOrigin: func(r *http.Request) bool { return true },
	}

	webClients    = make(map[*websocket.Conn]struct{})
	webMutex      sync.RWMutex
	cocAttributes = [...]string{"STR", "CON", "SIZ", "DEX", "APP", "INT", "POW", "EDU", "LUK"}
	qqManager     *ConnectionManager
	appConfig     *Config

	rollRegex = regexp.MustCompile(`^r\s*(\d+)d(\d+)([\+\-]\d+)?$`)
	scRegex   = regexp.MustCompile(`sc\s+(\d+)/(\d+)`)

	ErrMaxRetries  = errors.New("maximum retry attempts reached")
	ErrInvalidMsg  = errors.New("invalid message format")
	ErrConnClosed  = errors.New("connection closed")
	ErrConfigLoad  = errors.New("configuration load failed")
)

func NewConnectionManager(url string, maxRetry int) *ConnectionManager {
	return &ConnectionManager{
		url:      url,
		maxRetry: maxRetry,
		quit:     make(chan struct{}),
	}
}

func (cm *ConnectionManager) Connect() error {
	cm.mu.Lock()
	defer cm.mu.Unlock()

	if cm.conn != nil {
		return nil
	}

	var err error
	for i := 0; i < cm.maxRetry; i++ {
		cm.conn, _, err = websocket.DefaultDialer.Dial(cm.url, nil)
		if err == nil {
			cm.retries = 0
			return nil
		}

		select {
		case <-cm.quit:
			return ErrConnClosed
		default:
			waitTime := time.Duration(i+1) * time.Second
			log.Printf("连接失败 (尝试 %d/%d), %v秒后重试...", i+1, cm.maxRetry, waitTime)
			time.Sleep(waitTime)
		}
	}
	return fmt.Errorf("%w: %v", ErrMaxRetries, err)
}

func (cm *ConnectionManager) Get() (*websocket.Conn, error) {
	cm.mu.RLock()
	defer cm.mu.RUnlock()

	if cm.conn == nil {
		return nil, ErrConnClosed
	}
	return cm.conn, nil
}

func (cm *ConnectionManager) Close() {
	cm.mu.Lock()
	defer cm.mu.Unlock()

	close(cm.quit)
	if cm.conn != nil {
		cm.conn.Close()
		cm.conn = nil
	}
}

func loadConfig() (*Config, error) {
	var cfg Config
	if err := env.Parse(&cfg); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrConfigLoad, err)
	}

	if cfg.QQGroupID == 0 {
		return nil, fmt.Errorf("%w: QQ_GROUP_ID is required", ErrConfigLoad)
	}

	return &cfg, nil
}

func main() {
	rand.Seed(time.Now().UnixNano())

	var err error
	appConfig, err = loadConfig()
	if err != nil {
		log.Printf("配置加载错误: %v", err)
		log.Println("使用默认配置继续运行...")
		appConfig = &Config{
			HTTPPort:  "8088",
			QQWSURL:   "ws://127.0.0.1:3009",
			QQGroupID: 0, // 需要后续处理
		}
	}

	qqManager = NewConnectionManager(appConfig.QQWSURL, 5)
	defer qqManager.Close()

	if err := qqManager.Connect(); err != nil {
		log.Printf("初始化QQ连接失败: %v", err)
		log.Println("将在消息处理时尝试重新连接...")
	}

	go startHTTPServer()

	messageLoop()
}

func messageLoop() {
	for {
		conn, err := qqManager.Get()
		if err != nil {
			if err := qqManager.Connect(); err != nil {
				log.Printf("QQ连接不可用: %v", err)
				time.Sleep(5 * time.Second)
				continue
			}
			conn, _ = qqManager.Get()
		}

		_, message, err := conn.ReadMessage()
		if err != nil {
			log.Printf("读取消息错误: %v", err)
			qqManager.mu.Lock()
			qqManager.conn = nil
			qqManager.mu.Unlock()
			time.Sleep(1 * time.Second)
			continue
		}

		msg, err := parseIncomingMessage(message)
		if err != nil {
			log.Printf("消息解析错误: %v", err)
			continue
		}

		if msg.PostType == "message" {
			handleMessage(conn, msg)
		}
	}
}

func parseIncomingMessage(data []byte) (*OneBotMessage, error) {
	var msg OneBotMessage
	if err := json.Unmarshal(data, &msg); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidMsg, err)
	}
	return &msg, nil
}

func startHTTPServer() {
	http.HandleFunc("/", serveStatic)
	http.HandleFunc("/ws", handleWebSocket)
	log.Printf("Web服务器已启动 :%s", appConfig.HTTPPort)
	if err := http.ListenAndServe(":"+appConfig.HTTPPort, nil); err != nil {
		log.Fatalf("HTTP服务器错误: %v", err)
	}
}

func serveStatic(w http.ResponseWriter, r *http.Request) {
	path := filepath.Join("web", filepath.Clean(r.URL.Path))
	if _, err := os.Stat(path); os.IsNotExist(err) {
		http.ServeFile(w, r, filepath.Join("web", "index.html"))
		return
	}
	http.ServeFile(w, r, path)
}

func handleWebSocket(w http.ResponseWriter, r *http.Request) {
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("WebSocket升级失败: %v", err)
		return
	}

	webMutex.Lock()
	webClients[conn] = struct{}{}
	webMutex.Unlock()

	go func() {
		defer func() {
			webMutex.Lock()
			delete(webClients, conn)
			webMutex.Unlock()
			conn.Close()
		}()

		for {
			_, _, err := conn.ReadMessage()
			if err != nil {
				if !websocket.IsCloseError(err, websocket.CloseNormalClosure) {
					log.Printf("WebSocket读取错误: %v", err)
				}
				break
			}
		}
	}()
}

func handleMessage(conn *websocket.Conn, msg *OneBotMessage) {
	messageStr, err := extractMessageContent(msg.Message)
	if err != nil {
		log.Printf("消息内容提取失败: %v", err)
		return
	}

	if !strings.HasPrefix(messageStr, ".") {
		return
	}

	response := processCommand(messageStr)
	if err := sendResponse(conn, msg, response); err != nil {
		log.Printf("发送响应失败: %v", err)
		return
	}

	broadcastToWeb(formatWebMessage(msg, response))
}

func extractMessageContent(msg interface{}) (string, error) {
	switch m := msg.(type) {
	case string:
		return m, nil
	case []interface{}:
		var builder strings.Builder
		for _, v := range m {
			seg, ok := v.(map[string]interface{})
			if !ok {
				continue
			}

			if seg["type"] == "text" {
				data, ok := seg["data"].(map[string]interface{})
				if !ok {
					continue
				}

				text, ok := data["text"].(string)
				if ok {
					builder.WriteString(text)
				}
			}
		}
		return builder.String(), nil
	default:
		return "", fmt.Errorf("%w: unexpected type %T", ErrInvalidMsg, msg)
	}
}

func formatWebMessage(msg *OneBotMessage, response string) string {
	if msg.MessageType == "group" {
		return fmt.Sprintf("[群消息] %s", response)
	}
	return fmt.Sprintf("[私聊] %s", response)
}

func processCommand(cmd string) string {
	cmd = strings.TrimPrefix(cmd, ".")
	switch {
	case rollRegex.MatchString(cmd):
		return processRoll(cmd)
	case strings.HasPrefix(cmd, "coc"):
		return processCoC()
	case scRegex.MatchString(cmd):
		return processSanCheck(cmd)
	case cmd == "help":
		return "COC指令帮助:\n.r[骰子指令] 掷骰\n.coc 生成调查员\n.sc [成功损失]/[失败损失] 理智检定"
	default:
		return "未知指令，请输入.help查看帮助"
	}
}

func processRoll(cmd string) string {
	matches := rollRegex.FindStringSubmatch(cmd)
	if len(matches) < 3 {
		return "无效的骰子指令格式"
	}

	diceNum, _ := strconv.Atoi(matches[1])
	diceSides, _ := strconv.Atoi(matches[2])
	modifier := 0
	if len(matches) > 3 && matches[3] != "" {
		modifier, _ = strconv.Atoi(matches[3])
	}

	var rolls []int
	total := 0
	for i := 0; i < diceNum; i++ {
		roll := rand.Intn(diceSides) + 1
		rolls = append(rolls, roll)
		total += roll
	}
	total += modifier

	result := fmt.Sprintf("掷骰 %dd%d: %v", diceNum, diceSides, rolls)
	if modifier != 0 {
		result += fmt.Sprintf(" %+d = %d", modifier, total)
	}
	return result
}

func processCoC() string {
	var attributes []string
	for _, attr := range cocAttributes {
		var value int
		switch attr {
		case "STR", "CON", "SIZ", "DEX", "APP":
			value = rand.Intn(6)*5 + 30
		case "INT":
			value = rand.Intn(6)*5 + 50
		case "POW":
			value = rand.Intn(6)*5 + 40
		case "EDU":
			value = rand.Intn(6)*5 + 50
		case "LUK":
			value = rand.Intn(6)*5 + 30
		}
		attributes = append(attributes, fmt.Sprintf("%s: %d", attr, value))
	}
	return "调查员属性:\n" + strings.Join(attributes, "\n")
}

func processSanCheck(cmd string) string {
	matches := scRegex.FindStringSubmatch(cmd)
	if len(matches) < 3 {
		return "理智检定格式错误，正确格式：.sc 成功损失/失败损失"
	}
	successLoss, _ := strconv.Atoi(matches[1])
	failLoss, _ := strconv.Atoi(matches[2])

	roll := rand.Intn(100) + 1
	result := fmt.Sprintf("理智检定: %d/%d 掷骰: %d", successLoss, failLoss, roll)
	return result
}

func broadcastToWeb(message string) {
	webMutex.RLock()
	defer webMutex.RUnlock()

	for client := range webClients {
		client := client
		go func() {
			if err := client.WriteMessage(websocket.TextMessage, []byte(message)); err != nil {
				log.Printf("广播消息失败: %v", err)
				webMutex.Lock()
				delete(webClients, client)
				client.Close()
				webMutex.Unlock()
			}
		}()
	}
}

func handleWebCommand(command string) {
	response := processCommand(command)
	broadcastToWeb(response)

	if err := sendToQQ(response); err != nil {
		log.Printf("发送到QQ失败: %v", err)
	}
}

func sendToQQ(message string) error {
	conn, err := qqManager.Get()
	if err != nil {
		if err := qqManager.Connect(); err != nil {
			return fmt.Errorf("连接失败: %w", err)
		}
		conn, _ = qqManager.Get()
	}

	resp := ResponseMessage{
		Action: "send_msg",
		Params: map[string]interface{}{
			"message_type": "group",
			"group_id":     appConfig.QQGroupID,
			"message":      message,
		},
	}

	if err := conn.WriteJSON(resp); err != nil {
		qqManager.mu.Lock()
		qqManager.conn = nil
		qqManager.mu.Unlock()
		return fmt.Errorf("写入失败: %w", err)
	}
	return nil
}

func sendResponse(conn *websocket.Conn, msg *OneBotMessage, response string) error {
	params := map[string]interface{}{
		"message_type": msg.MessageType,
		"message":      response,
	}

	if msg.MessageType == "group" {
		params["group_id"] = msg.GroupID
	} else {
		params["user_id"] = msg.UserID
	}

	resp := ResponseMessage{
		Action: "send_msg",
		Params: params,
	}

	if err := conn.WriteJSON(resp); err != nil {
		return fmt.Errorf("发送响应失败: %w", err)
	}
	return nil
}