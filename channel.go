package iowsock

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"golang.org/x/net/websocket"
	"log"
	"time"
)

type ChannelMessage struct {
	Time    int64       `json:"time"`
	From    string      `json:"from"`
	Body    interface{} `json:"body"`
	Publish bool        `json:"publish"`
}

type ChannelCallback interface {
	OnAdd(id, addr string, num int)
	OnDel(id, addr string, num int)
	OnSend(from, to string, body interface{})
}

type Channel struct {
	ID       string
	Handler  websocket.Handler
	token    string
	callback ChannelCallback
	block    cipher.Block
	clientID int
	clients  map[int]*ChannelClient
	msgCh    chan *ChannelMessage
	addCh    chan *ChannelClient
	delCh    chan *ChannelClient
	doneCh   chan bool
}

type ChannelClientStat struct {
	Addr          string `json:"addr"`
	LastAcitivity int    `json:"lastActivity"`
}

func NewChannel(callback ChannelCallback, id string) *Channel {
	ch := &Channel{callback: callback, ID: id, clients: make(map[int]*ChannelClient), msgCh: make(chan *ChannelMessage), addCh: make(chan *ChannelClient), delCh: make(chan *ChannelClient), doneCh: make(chan bool)}
	ch.Handler = websocket.Handler(ch.handler)
	return ch
}

func (s *Channel) SetKey(hexKey string) error {
	if hexKey == "" {
		s.block = nil
		return nil
	}
	key, err := hex.DecodeString(hexKey)
	if err != nil {
		return err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return err
	}
	s.block = block
	return nil
}

func (s *Channel) SetToken(token string) {
	s.token = token
}

func (s *Channel) Close() {
	s.doneCh <- true
}

func (s *Channel) Stat() []*ChannelClientStat {
	stats := make([]*ChannelClientStat, 0)
	for _, c := range s.clients {
		stats = append(stats, &ChannelClientStat{Addr: c.conn.Request().RemoteAddr, LastAcitivity: int(time.Now().Sub(c.lastActivity).Seconds())})
	}
	return stats
}

func (s *Channel) add(c *ChannelClient) {
	s.addCh <- c
}

func (s *Channel) del(c *ChannelClient) {
	s.delCh <- c
}

func (s *Channel) Send(msg *ChannelMessage) {
	s.msgCh <- msg
}

func (s *Channel) handler(conn *websocket.Conn) {
	log.Println(fmt.Sprintf("Websocket connected: %s", conn.RemoteAddr().String()))
	client := newChannelClient(s.ID, s.token, s.clientID, s.block, s.callback, conn)
	s.clientID++
	s.add(client)
	client.closeCb = func() {
		s.del(client)
	}
	client.listen()
}

// Listen and serve.
// It serves client connection and broadcast request.
func (s *Channel) Listen() {
	log.Println(fmt.Sprintf("Accepting websocket: %s", s.ID))
	for {
		select {
		// Add new a client
		case c := <-s.addCh:
			log.Printf("Adding client: %s", c.String())
			s.clients[c.ID] = c
			n := len(s.clients)
			log.Printf("Channel %s: %d clients", s.ID, n)
			go s.callback.OnAdd(s.ID, c.conn.Request().RemoteAddr, n)
			//s.sendPastMessages(c)
		// del a client
		case c := <-s.delCh:
			log.Printf("Deleting client: %s", c.String())
			delete(s.clients, c.ID)
			n := len(s.clients)
			go s.callback.OnDel(s.ID, c.conn.Request().RemoteAddr, n)
		// broadcast message for all clients
		case msg := <-s.msgCh:
			delivered := false
			for _, c := range s.clients {
				if c.write(msg) == nil {
					delivered = true
				}
			}
			if !delivered {
				log.Printf("Message not delivered to %s", s.ID)
			}
		case <-s.doneCh:
			for id, c := range s.clients {
				c.Close()
				delete(s.clients, id)
			}
			log.Printf("Done")
			return
		}
	}
}

type ChannelClient struct {
	UID          string
	ID           int
	token        string
	block        cipher.Block
	authorized   bool
	callback     ChannelCallback
	conn         *websocket.Conn
	closeCb      func()
	msgCh        chan interface{}
	closeCh      chan bool
	ticker       *time.Ticker
	tickerStop   chan bool
	timeout      chan bool
	lastActivity time.Time
}

func newChannelClient(uid, token string, id int, block cipher.Block, callback ChannelCallback, conn *websocket.Conn) *ChannelClient {
	return &ChannelClient{UID: uid, ID: id, token: token, block: block, authorized: block != nil, callback: callback, conn: conn, timeout: make(chan bool), msgCh: make(chan interface{}, BufferLevel), closeCh: make(chan bool),
		lastActivity: time.Now()}
}

func (c *ChannelClient) Close() {
	log.Printf("Closing channel client: %s", c.String())
	c.closeCb = nil
	c.onClose()
}

func (c *ChannelClient) String() string {
	return fmt.Sprintf("%s[%d]@%s", c.UID, c.ID, c.conn.Request().RemoteAddr)
}

func (c *ChannelClient) send(to string, body map[string]interface{}) {
	c.callback.OnSend(c.UID, to, body)
}

func (c *ChannelClient) write(msg *ChannelMessage) error {
	select {
	case c.msgCh <- msg:
		return nil
	default:
		log.Printf("Buffer overflows %s: %d", c.String(), BufferLevel)
		return fmt.Errorf("Buffer overflows")
	}
}

// Listen Write and Read request via chanel
func (c *ChannelClient) listen() {
	c.ticker = time.NewTicker(HeartbeatCheck)
	c.tickerStop = make(chan bool)
	go func() {
		defer c.ticker.Stop()
		for {
			select {
			case <-c.ticker.C:
				if time.Now().Sub(c.lastActivity) > HeartbeatTimeout {
					c.timeout <- true
				}
			case <-c.tickerStop:
				return
			}
		}
	}()
	go c.listenRead()
	c.listenWrite()
}

func (c *ChannelClient) onClose() {
	select {
	case c.tickerStop <- true:
	default:
	}
	select {
	case c.closeCh <- true:
	default:
	}
	if c.closeCb != nil {
		c.closeCb()
	}
}

func (c *ChannelClient) listenWrite() {
	defer c.closeRead()
	log.Printf("Awaiting for write web socket: %s", c.String())
	for {
		select {
		case msg := <-c.msgCh:
			if !c.authorized {
				log.Printf("Not authorized")
				continue
			}
			plaintext, err := json.Marshal(msg)
			if err != nil {
				log.Printf("Failed to marshal: %s", err.Error())
				continue
			}
			if c.block == nil {
				err = websocket.JSON.Send(c.conn, msg)
			} else {
				blockSize := c.block.BlockSize()
				paddedtext := PKCS5.Padding(plaintext, blockSize)
				ciphertext := make([]byte, blockSize+len(paddedtext))
				iv := ciphertext[:blockSize]
				_, err = rand.Read(iv)
				if err != nil {
					log.Printf("Failed to generate nonce: %s", err.Error())
					continue
				}
				mode := cipher.NewCBCEncrypter(c.block, iv)
				mode.CryptBlocks(ciphertext[blockSize:], paddedtext)
				err = websocket.Message.Send(c.conn, ciphertext)
			}
			if err != nil {
				log.Printf("Failed to send to %s: %s", c.String(), err.Error())
				return
				// } else {
				// 	log.Printf("Delivered to %s: %s", c.String(), string(plaintext))
			}
		case <-c.closeCh:
			return
		case <-c.timeout:
			log.Printf("Client %s timed out", c.String())
			return
		}
	}
}

func (c *ChannelClient) closeWrite() {
	c.closeCh <- true
}

func (c *ChannelClient) listenRead() {
	defer c.onClose()
	log.Printf("Reading web socket: %s", c.String())
	for {
		var ciphertext, plaintext []byte
		err := websocket.Message.Receive(c.conn, &ciphertext)
		if err != nil {
			log.Printf("Failed to receive from %s: %s", c.String(), err.Error())
			return
		}
		if c.block == nil {
			plaintext = ciphertext
		} else {
			blockSize := c.block.BlockSize()
			if len(ciphertext) < blockSize {
				log.Printf("Too short cipher text from %s", c.String())
				continue
			}
			iv := ciphertext[:blockSize]
			ciphertext = ciphertext[blockSize:]
			if len(ciphertext)%blockSize != 0 {
				log.Printf("Invalid length of cipher text from %s", c.String())
				continue
			}
			mode := cipher.NewCBCDecrypter(c.block, iv)
			plaintext = make([]byte, len(ciphertext))
			mode.CryptBlocks(plaintext, ciphertext)
			plaintext, err = PKCS5.Unpadding(plaintext, blockSize)
			if err != nil {
				log.Printf("Failed to unpadding from %s: %s", c.String(), err.Error())
				continue
			}
		}
		msg := &EventMessage{}
		err = json.Unmarshal(plaintext, msg)
		if err != nil {
			log.Printf("Failed to unmarshal message from %s: %s", c.String(), err.Error())
			continue
		}
		if c.authorized {
			c.lastActivity = time.Now()
			switch msg.Event {
			case "send":
				to := msg.Body["to"]
				body := msg.Body["body"]
				c.send(to.(string), body.(map[string]interface{}))
			case "echo":
				c.send(c.UID, msg.Body)
			case "heartbeat":
			default:
				log.Printf("Unknown event from client %s: %s", c.String(), msg.Event)
			}
		} else {
			switch msg.Event {
			case "auth":
				token := msg.Body["token"]
				if c.token == token {
					log.Printf("Authorized: %s", c.UID)
					c.lastActivity = time.Now()
					c.authorized = true
				}
			}
		}
	}
}

func (c *ChannelClient) closeRead() {
	c.conn.Close()
}
