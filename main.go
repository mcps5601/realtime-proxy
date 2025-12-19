package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"log"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/joho/godotenv"
)

const (
	openAIRealtimeURL = "wss://api.openai.com/v1/realtime?model=gpt-realtime"

	// Realtime audio/pcm rate 最低 >= 24000（你已經踩過 16000 會被拒絕）
	rateHz = 24000
	ch     = 1

	// WS keepalive
	pongWait   = 30 * time.Second
	pingPeriod = 10 * time.Second
	writeWait  = 5 * time.Second
)

var upgrader = websocket.Upgrader{
	ReadBufferSize:  4096,
	WriteBufferSize: 4096,
	CheckOrigin:     func(r *http.Request) bool { return true },
}

type openAIEvent map[string]any

func main() {
	_ = godotenv.Load() // 沒有 .env 也沒關係

	if os.Getenv("OPENAI_API_KEY") == "" {
		log.Fatal("missing env OPENAI_API_KEY")
	}

	http.HandleFunc("/ws", handleClientWS)
	log.Println("listening on :8080/ws")
	log.Fatal(http.ListenAndServe(":8080", nil))
}

func handleClientWS(w http.ResponseWriter, r *http.Request) {
	clientConn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Println("upgrade error:", err)
		return
	}
	defer clientConn.Close()
	log.Println("client connected")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// ---- client keepalive (重要：要跟其他 Write 共用同一把鎖，避免 concurrent write) ----
	var clientWriteMu sync.Mutex

	clientConn.SetReadLimit(8 * 1024 * 1024)
	_ = clientConn.SetReadDeadline(time.Now().Add(pongWait))
	clientConn.SetPongHandler(func(string) error {
		_ = clientConn.SetReadDeadline(time.Now().Add(pongWait))
		return nil
	})
	go pingLoop(ctx, clientConn, &clientWriteMu)

	// ---- connect to OpenAI Realtime ----
	openaiConn, err := dialOpenAIRealtime()
	if err != nil {
		log.Println("dial openai error:", err)
		return
	}
	defer openaiConn.Close()

	// OpenAI read deadline / pong
	openaiConn.SetReadLimit(8 * 1024 * 1024)
	_ = openaiConn.SetReadDeadline(time.Now().Add(pongWait))
	openaiConn.SetPongHandler(func(string) error {
		_ = openaiConn.SetReadDeadline(time.Now().Add(pongWait))
		return nil
	})

	// ✅ 單一 writer：所有送給 OpenAI 的訊息（含 ping）都走這個 writer
	openaiWriter := NewWSWriter(ctx, openaiConn)

	// ---- session.update：開 server VAD + create_response=true（你就不用自己 commit/response.create）----
	openaiWriter.SendJSON(openAIEvent{
		"type": "session.update",
		"session": openAIEvent{
			"type":         "realtime",
			"instructions": "請用中文與使用者自然對話，回覆以語音為主。",
			"output_modalities": []string{
				"audio",
			},
			"audio": openAIEvent{
				"input": openAIEvent{
					"format": openAIEvent{"type": "audio/pcm", "rate": rateHz},
					"turn_detection": openAIEvent{
						"type":                "server_vad",
						"threshold":           0.5,
						"prefix_padding_ms":   300,
						"silence_duration_ms": 600,
						"create_response":     true, // ✅ 關鍵：自動產生回覆
					},
				},
				"output": openAIEvent{
					"format": openAIEvent{"type": "audio/pcm", "rate": rateHz},
					"voice":  "marin",
					"speed":  1,
				},
			},
		},
	})
	log.Println("→ session.update sent (server VAD enabled)")

	// ---- OpenAI receiver：收到 audio delta 就轉回 binary 給 client ----
	go func() {
		for {
			_, msg, err := openaiConn.ReadMessage()
			if err != nil {
				// 這通常是你 cancel / conn close 造成的，屬於正常收尾
				log.Println("openai read error:", err)
				cancel()
				return
			}

			var evt openAIEvent
			if err := json.Unmarshal(msg, &evt); err != nil {
				log.Println("openai json error:", err)
				continue
			}

			t, _ := evt["type"].(string)

			switch t {
			case "error":
				pretty, _ := json.MarshalIndent(evt, "", "  ")
				log.Printf("❌ openai error event:\n%s\n", string(pretty))

				// 把 error 也丟回 client（文字）
				clientWriteMu.Lock()
				_ = clientConn.WriteMessage(websocket.TextMessage, pretty)
				clientWriteMu.Unlock()

			case "response.output_audio.delta":
				delta, _ := evt["delta"].(string)
				pcm, err := base64.StdEncoding.DecodeString(delta)
				if err != nil {
					log.Println("decode delta error:", err)
					continue
				}

				clientWriteMu.Lock()
				_ = clientConn.WriteMessage(websocket.BinaryMessage, pcm)
				clientWriteMu.Unlock()

			case "response.done":
				log.Println("🟢 response.done")

			default:
				// 初期你想觀察事件就留著；穩定後可註解掉避免洗版
				log.Println("openai event:", t)
			}
		}
	}()

	// ---- Client → OpenAI：binary audio 直接 append（不再做 idle commit）----
	for {
		msgType, data, err := clientConn.ReadMessage()
		if err != nil {
			if websocket.IsCloseError(err,
				websocket.CloseNormalClosure,
				websocket.CloseGoingAway,
				websocket.CloseNoStatusReceived,
			) {
				log.Println("client disconnected")
			} else {
				log.Println("client read error:", err)
			}
			cancel()
			return
		}

		switch msgType {
		case websocket.BinaryMessage:
			// 直接 append。server VAD 會自己決定何時 commit 並自動回覆
			openaiWriter.SendJSON(openAIEvent{
				"type":  "input_audio_buffer.append",
				"audio": base64.StdEncoding.EncodeToString(data),
			})

		case websocket.TextMessage:
			// debug/控制命令（可選）
			cmd := string(data)
			switch cmd {
			case "clear":
				log.Println("→ cmd clear")
				openaiWriter.SendJSON(openAIEvent{"type": "input_audio_buffer.clear"})

			case "cancel":
				log.Println("→ cmd response.cancel")
				openaiWriter.SendJSON(openAIEvent{"type": "response.cancel"})

			case "force":
				// 可選：強制讓模型開始回（有時你想立即回不想等 VAD）
				log.Println("→ cmd response.create (force)")
				openaiWriter.SendJSON(openAIEvent{
					"type":     "response.create",
					"response": openAIEvent{"output_modalities": []string{"audio"}},
				})

			default:
				log.Println("client text:", cmd)
			}
		}
	}
}

func dialOpenAIRealtime() (*websocket.Conn, error) {
	apiKey := os.Getenv("OPENAI_API_KEY")
	h := http.Header{}
	h.Set("Authorization", "Bearer "+apiKey)

	conn, _, err := websocket.DefaultDialer.Dial(openAIRealtimeURL, h)
	return conn, err
}

// ---- 單一 Writer（含 ping）----
// gorilla/websocket：同一條連線只允許一個 goroutine 寫入，這個結構就是為了解決它
type WSWriter struct {
	conn *websocket.Conn
	ch   chan []byte
}

func NewWSWriter(ctx context.Context, conn *websocket.Conn) *WSWriter {
	w := &WSWriter{
		conn: conn,
		ch:   make(chan []byte, 512),
	}

	go func() {
		t := time.NewTicker(pingPeriod)
		defer t.Stop()

		for {
			select {
			case <-ctx.Done():
				return

			case b := <-w.ch:
				_ = w.conn.SetWriteDeadline(time.Now().Add(writeWait))
				if err := w.conn.WriteMessage(websocket.TextMessage, b); err != nil {
					return
				}

			case <-t.C:
				_ = w.conn.SetWriteDeadline(time.Now().Add(writeWait))
				if err := w.conn.WriteMessage(websocket.PingMessage, nil); err != nil {
					return
				}
			}
		}
	}()

	return w
}

func (w *WSWriter) SendJSON(v any) {
	b, _ := json.Marshal(v)
	// 這裡用阻塞，確保不丟控制訊息；要更低延遲可改成滿了就丟 audio（但不丟 control）
	w.ch <- b
}

// clientConn 的 ping loop：注意要用同一把 clientWriteMu
func pingLoop(ctx context.Context, conn *websocket.Conn, mu *sync.Mutex) {
	t := time.NewTicker(pingPeriod)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			_ = conn.SetWriteDeadline(time.Now().Add(writeWait))
			mu.Lock()
			err := conn.WriteMessage(websocket.PingMessage, nil)
			mu.Unlock()
			if err != nil {
				return
			}
		}
	}
}
