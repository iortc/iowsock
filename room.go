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

type RoomMessage struct {
	From string      `json:"from"`
	To   string      `json:"to,omitempty"`
	Body interface{} `json:"body"`
}

type RoomCallback interface {
	OnAdd(id, addr string)
	OnDel(id, addr string)
}

type Room struct {
	ID       string
	Handler  websocket.Handler
	block    cipher.Block
	callback RoomCallback
	clientID int
	clients  map[int]*RoomClient
	msgCh    chan *RoomMessage
	addCh    chan *RoomClient
	delCh    chan *RoomClient
	doneCh   chan bool
}

type RoomClientStat struct {
	Addr          string `json:"addr"`
	LastAcitivity int    `json:"lastActivity"`
}

func NewRoom(callback RoomCallback, id string) *Room {
	rm := &Room{callback: callback, ID: id, clients: make(map[int]*RoomClient), msgCh: make(chan *RoomMessage), addCh: make(chan *RoomClient), delCh: make(chan *RoomClient), doneCh: make(chan bool)}
	rm.Handler = websocket.Handler(rm.handler)
	return rm
}

func (s *Room) SetKey(hexKey string) error {
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

func (s *Room) Close() {
	for id, c := range s.clients {
		c.Close()
		delete(s.clients, id)
	}
	s.doneCh <- true
}

func (s *Room) Stat() []*RoomClientStat {
	stats := make([]*RoomClientStat, 0)
	for _, c := range s.clients {
		stats = append(stats, &RoomClientStat{Addr: c.conn.Request().RemoteAddr, LastAcitivity: int(time.Now().Sub(c.lastActivity).Seconds())})
	}
	return stats
}

func (s *Room) add(c *RoomClient) {
	s.addCh <- c
}

func (s *Room) del(c *RoomClient) {
	s.delCh <- c
}

func (s *Room) broadcast(src *RoomClient, msg *RoomMessage) {
	for _, c := range s.clients {
		c.write(msg)
	}
}

func (s *Room) handler(conn *websocket.Conn) {
	log.Println(fmt.Sprintf("Websocket connected: %s", conn.RemoteAddr().String()))
	client := newRoomClient(s.clientID, s.block, s, conn)
	s.clientID++
	s.add(client)
	client.closeCb = func() {
		s.del(client)
	}
	client.listen()
}

// Listen and serve.
// It serves client connection and broadcast request.
func (s *Room) Listen() {
	log.Println(fmt.Sprintf("Accepting websocket: %s", s.ID))
	for {
		select {
		// Add new a client
		case c := <-s.addCh:
			log.Printf("Adding client: %s", c.String())
			s.clients[c.ID] = c
			log.Printf("Room %s: %d clients", s.ID, len(s.clients))
			go s.callback.OnAdd(s.ID, c.conn.Request().RemoteAddr)
			//s.sendPastMessages(c)
		// del a client
		case c := <-s.delCh:
			log.Printf("Deleting client: %s", c.String())
			delete(s.clients, c.ID)
			go s.callback.OnDel(s.ID, c.conn.Request().RemoteAddr)
		// broadcast message for all clients
		case msg := <-s.msgCh:
			for _, c := range s.clients {
				c.write(msg)
			}
		case <-s.doneCh:
			log.Printf("Done")
			return
		}
	}
}

type RoomClient struct {
	ID           int
	block        cipher.Block
	room         *Room
	conn         *websocket.Conn
	closeCb      func()
	msgCh        chan interface{}
	closeCh      chan bool
	ticker       *time.Ticker
	timeout      chan bool
	lastActivity time.Time
}

func newRoomClient(id int, block cipher.Block, room *Room, conn *websocket.Conn) *RoomClient {
	return &RoomClient{ID: id, block: block, room: room, conn: conn, timeout: make(chan bool), msgCh: make(chan interface{}, BufferLevel), closeCh: make(chan bool),
		lastActivity: time.Now()}
}

func (c *RoomClient) Close() {
	log.Printf("Closing room client: %s", c.String())
	c.onClose()
}

func (c *RoomClient) String() string {
	return fmt.Sprintf("[%d]@%s", c.ID, c.conn.Request().RemoteAddr)
}

func (c *RoomClient) broadcast(msg *RoomMessage) {
	c.room.broadcast(c, msg)
}

func (c *RoomClient) write(msg *RoomMessage) error {
	select {
	case c.msgCh <- msg:
		return nil
	default:
		log.Printf("Buffer overflows %s: %d", c.String(), BufferLevel)
		return fmt.Errorf("Buffer overflows")
	}
}

// Listen Write and Read request via chanel
func (c *RoomClient) listen() {
	c.ticker = time.NewTicker(HeartbeatCheck)
	go func() {
		for _ = range c.ticker.C {
			if time.Now().Sub(c.lastActivity) > HeartbeatTimeout {
				c.timeout <- true
			}
		}
	}()
	go c.listenRead()
	c.listenWrite()
}

func (c *RoomClient) onClose() {
	c.ticker.Stop()
	select {
	case c.closeCh <- true:
	default:
	}
	if c.closeCb != nil {
		c.closeCb()
	}
}

func (c *RoomClient) listenWrite() {
	defer c.closeRead()
	log.Printf("Awaiting for write web socket: %s", c.String())
	for {
		select {
		case msg := <-c.msgCh:
			plaintext, err := json.Marshal(msg)
			if err != nil {
				log.Printf("Failed to marshal: %s", err.Error())
				continue
			}
			if c.block == nil {
				err = websocket.JSON.Send(c.conn, msg)
			} else {
				blockSize := c.block.BlockSize()
				plaintext = PKCS5.Padding(plaintext, blockSize)
				ciphertext := make([]byte, blockSize+len(plaintext))
				iv := ciphertext[:blockSize]
				_, err = rand.Read(iv)
				if err != nil {
					log.Printf("Failed to generate nonce: %s", err.Error())
					continue
				}
				mode := cipher.NewCBCEncrypter(c.block, iv)
				mode.CryptBlocks(ciphertext[blockSize:], plaintext)
				err = websocket.Message.Send(c.conn, ciphertext)
			}
			if err != nil {
				log.Printf("Failed to send to %s: %s", c.String(), err.Error())
				return
			}
		case <-c.closeCh:
			return
		case <-c.timeout:
			log.Printf("Client %s timed out\n", c.String())
			return
		}
	}
}

func (c *RoomClient) closeWrite() {
	c.closeCh <- true
}

func (c *RoomClient) listenRead() {
	defer c.onClose()
	log.Printf("Reading web socket: %s", c.String())
	for {
		var ciphertext, plaintext []byte
		err := websocket.Message.Receive(c.conn, &ciphertext)
		if err != nil {
			log.Printf("Failed to receive from %s: %s\n", c.String(), err.Error())
			return
		}
		if c.block == nil {
			plaintext = ciphertext
		} else {
			blockSize := c.block.BlockSize()
			if len(ciphertext) < blockSize {
				log.Printf("Too short cipher text from %s\n", c.String())
				continue
			}
			iv := ciphertext[:blockSize]
			ciphertext = ciphertext[blockSize:]
			if len(ciphertext)%blockSize != 0 {
				log.Printf("Invalid length of cipher text from %s\n", c.String())
				continue
			}
			mode := cipher.NewCBCDecrypter(c.block, iv)
			plaintext = make([]byte, len(ciphertext))
			mode.CryptBlocks(plaintext, ciphertext)
			plaintext, err = PKCS5.Unpadding(plaintext, blockSize)
			if err != nil {
				log.Printf("Failed to unpadding from %s: %s\n", c.String(), err.Error())
				continue
			}
		}
		msg := &RoomMessage{}
		err = json.Unmarshal(plaintext, msg)
		if err != nil {
			log.Printf("Failed to unmarshal message from %s: %s\n", c.String(), err.Error())
			continue
		}
		if msg.From == "" {
			log.Printf("Invalid message from %s\n", c.String())
			continue
		}
		c.lastActivity = time.Now()
		c.broadcast(msg)
	}
}

func (c *RoomClient) closeRead() {
	c.conn.Close()
}
