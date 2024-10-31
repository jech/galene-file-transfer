package main

import (
	"bytes"
	crand "crypto/rand"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/gorilla/websocket"
	"github.com/pion/webrtc/v3"
)

type groupStatus struct {
	Name        string `json:"name"`
	Redirect    string `json:"redirect,omitempty"`
	Location    string `json:"location,omitempty"`
	Endpoint    string `json:"endpoint,omitempty"`
	DisplayName string `json:"displayName,omitempty"`
	Description string `json:"description,omitempty"`
	AuthServer  string `json:"authServer,omitempty"`
	AuthPortal  string `json:"authPortal,omitempty"`
	Locked      bool   `json:"locked,omitempty"`
	ClientCount *int   `json:"clientCount,omitempty"`
}

type clientMessage struct {
	Type             string                   `json:"type"`
	Version          []string                 `json:"version,omitempty"`
	Kind             string                   `json:"kind,omitempty"`
	Error            string                   `json:"error,omitempty"`
	Id               string                   `json:"id,omitempty"`
	Replace          string                   `json:"replace,omitempty"`
	Source           string                   `json:"source,omitempty"`
	Dest             string                   `json:"dest,omitempty"`
	Username         *string                  `json:"username,omitempty"`
	Password         string                   `json:"password,omitempty"`
	Token            string                   `json:"token,omitempty"`
	Privileged       bool                     `json:"privileged,omitempty"`
	Permissions      []string                 `json:"permissions,omitempty"`
	Status           *groupStatus             `json:"status,omitempty"`
	Data             map[string]any           `json:"data,omitempty"`
	Group            string                   `json:"group,omitempty"`
	Value            any                      `json:"value,omitempty"`
	NoEcho           bool                     `json:"noecho,omitempty"`
	Time             string                   `json:"time,omitempty"`
	SDP              string                   `json:"sdp,omitempty"`
	Candidate        *webrtc.ICECandidateInit `json:"candidate,omitempty"`
	Label            string                   `json:"label,omitempty"`
	Request          any                      `json:"request,omitempty"`
	RTCConfiguration *webrtc.Configuration    `json:"rtcConfiguration,omitempty"`
}

var myId string
var client http.Client

var outfile string
var rtcConfiguration *webrtc.Configuration
var insecure, debug bool

func main() {
	var username, password, toUsername string
	var persist bool
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr,
			"Usage: %s group [file...]\n", os.Args[0],
		)
		flag.PrintDefaults()
	}
	flag.StringVar(&username, "username", "file-transfer",
		"`username` to use for login")
	flag.StringVar(&password, "password", "",
		"`password` to use for login")
	flag.StringVar(&toUsername, "to", "",
		"`username` to send files to")
	flag.StringVar(&outfile, "o", "",
		"output `filename` or directory")
	flag.BoolVar(&persist, "persist", false,
		"receive multiple files")
	flag.BoolVar(&insecure, "insecure", false,
		"don't check server certificates")
	flag.BoolVar(&debug, "debug", false,
		"enable protocol logging")
	flag.Parse()

	if flag.NArg() < 1 {
		flag.Usage()
		os.Exit(1)
	}

	sending := false
	if flag.NArg() > 1 {
		if toUsername == "" {
			log.Fatal("Need destination username for sending")
		}
		sending = true
	} else {
		if toUsername != "" {
			log.Fatal("No files to send")
		}
	}

	dialer := websocket.DefaultDialer
	if insecure {
		t := http.DefaultTransport.(*http.Transport).Clone()
		t.TLSClientConfig = &tls.Config{InsecureSkipVerify: true}
		client.Transport = t

		d := *dialer
		d.TLSClientConfig = &tls.Config{InsecureSkipVerify: true}
		dialer = &d
	}

	group, err := url.Parse(flag.Arg(0))
	if err != nil {
		log.Fatalf("Parse group: %v", err)
	}
	token := group.Query().Get("token")
	group.RawQuery = ""

	status, err := getGroupStatus(group.String())
	if err != nil {
		log.Fatalf("Get group status: %v", err)
	}

	if token == "" && status.AuthServer != "" {
		var err error
		token, err = getToken(
			status.AuthServer, group.String(), username, password,
		)
		if err != nil {
			log.Fatalf("Get token: %v", err)
		}
	}

	if status.Endpoint == "" {
		log.Fatalf("Server didn't provide endpoint.")
	}

	ws, _, err := dialer.Dial(status.Endpoint, nil)
	if err != nil {
		log.Fatalf("Connect to server: %v", err)
	}
	defer ws.Close()

	writer := newWriter[*clientMessage]()
	go writerLoop(ws, writer)

	readerCh := make(chan *clientMessage, 1)
	go readerLoop(ws, readerCh)

	myId = makeId()

	writer.write(&clientMessage{
		Type:    "handshake",
		Version: []string{"2", "1"},
		Id:      myId,
	})

	m := <-readerCh
	if m == nil {
		log.Fatal("Connection closed")
		return
	}
	if m.Type != "handshake" {
		log.Fatalf("Unexpected message %v", m.Type)
	}

	m = &clientMessage{
		Type:     "join",
		Kind:     "join",
		Group:    status.Name,
		Username: &username,
	}
	if token != "" {
		m.Token = token
	} else if password != "" {
		// don't leak passwords if we obtained a token
		m.Password = password
	}
	writer.write(m)

	done := make(chan struct{})

	terminate := make(chan os.Signal, 1)
	signal.Notify(terminate, syscall.SIGINT, syscall.SIGTERM)

	sent := false
outer:
	for {
		select {
		case <-terminate:
			break outer
		case <-done:
			break outer
		case m = <-readerCh:
			if m == nil {
				log.Println("Connection closed")
				break outer
			}
		}

		switch m.Type {
		case "joined":
			switch m.Kind {
			case "fail":
				log.Printf("Couldn't join: %v", m.Value)
				break outer
			case "join", "change":
				rtcConfiguration = m.RTCConfiguration
			case "leave":
				rtcConfiguration = nil
				break outer
			}
		case "usermessage":
			if m.Dest != myId {
				log.Println("Misdirected usermessage")
				break
			}
			if m.Kind == "error" || m.Kind == "warning" {
				log.Printf("%v: %v", m.Kind, m.Value)
				break
			}
			if m.Kind != "filetransfer" {
				log.Println("Unexpected usermessage", m.Kind)
				break
			}
			value, ok := m.Value.(map[string]any)
			if !ok {
				log.Println("Unexpected type for value")
				break
			}
			tpe, ok := value["type"].(string)
			if !ok {
				log.Println("Unexpected type for type")
				break
			}
			if tpe == "invite" {
				if sending {
					writer.write(&clientMessage{
						Type:   "usermessage",
						Source: myId,
						Dest:   m.Source,
						Kind:   "filetransfer",
						Value: map[string]any{
							"type": "abort",
							"id":   value["id"],
							"value": "this peer " +
								"does not" +
								"receive files",
						},
					})
					continue outer
				}
				go func() {
					err := receiveFile(
						writer, m.Source, value,
					)
					if err != nil {
						log.Printf("Receive: %v", err)
					}
					if !persist {
						close(done)
					}
				}()
			} else {
				err := gotMessage(writer, m.Source, value)
				if err != nil {
					log.Printf("File transfer: %v", err)
					break outer
				}
			}
		case "user":
			switch m.Kind {
			case "delete":
				abortTransfers(m.Id)
			case "add", "change":
				if m.Id == myId {
					continue outer
				}
				if sending && !sent && m.Username != nil &&
					*m.Username == toUsername {
					sent = true
					go func(filenames []string) {
						sendFiles(
							writer, m.Id, filenames,
						)
						if !persist {
							close(done)
						}
					}(flag.Args()[1:])
				}
			}
		case "ping":
			writer.write(&clientMessage{
				Type: "pong",
			})
		}
	}

	abortTransfers("")
	close(writer.ch)
	<-writer.done
}

func debugf(fmt string, args ...interface{}) {
	if debug {
		log.Printf(fmt, args...)
	}
}

func makeId() string {
	rawId := make([]byte, 8)
	crand.Read(rawId)
	return base64.RawURLEncoding.EncodeToString(rawId)
}

func readerLoop(ws *websocket.Conn, ch chan<- *clientMessage) {
	defer close(ch)
	for {
		var m clientMessage
		err := ws.ReadJSON(&m)
		if err != nil {
			debugf("ReadJSON: %v", err)
			return
		}
		if debug {
			j, _ := json.Marshal(m)
			debugf("<- %v", string(j))
		}
		ch <- &m
	}
}

type writer[T any] struct {
	ch   chan T
	done chan struct{}
}

func newWriter[T any]() *writer[T] {
	return &writer[T]{
		ch:   make(chan T, 8),
		done: make(chan struct{}),
	}
}

func (writer *writer[T]) write(m T) error {
	select {
	case writer.ch <- m:
		return nil
	case <-writer.done:
		return io.EOF
	}
}

func writerLoop(ws *websocket.Conn, writer *writer[*clientMessage]) {
	defer close(writer.done)
	for {
		m, ok := <-writer.ch
		if !ok {
			break
		}
		if debug {
			j, _ := json.Marshal(m)
			debugf("-> %v", string(j))
		}
		err := ws.SetWriteDeadline(time.Now().Add(10 * time.Second))
		if err != nil {
			debugf("Writer deadline: %v", err)
			return
		}
		err = ws.WriteJSON(m)
		if err != nil {
			debugf("WriteJSON: %v", err)
			return
		}
	}
	ws.WriteControl(websocket.CloseMessage,
		websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""),
		time.Now().Add(100*time.Millisecond),
	)
}

func getGroupStatus(group string) (groupStatus, error) {
	s, err := url.Parse(group)
	if err != nil {
		return groupStatus{}, err
	}
	s.Path = path.Join(s.Path, ".status.json")
	s.RawPath = ""
	resp, err := client.Get(s.String())
	if err != nil {
		return groupStatus{}, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return groupStatus{}, errors.New(resp.Status)
	}

	decoder := json.NewDecoder(resp.Body)
	var status groupStatus
	err = decoder.Decode(&status)
	if err != nil {
		return groupStatus{}, err
	}
	return status, nil
}

func getToken(server, group, username, password string) (string, error) {
	request := map[string]any{
		"username": username,
		"location": group,
		"password": password,
	}
	req, err := json.Marshal(request)
	if err != nil {
		return "", err
	}
	resp, err := client.Post(
		server, "application/json", bytes.NewReader(req),
	)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNoContent {
		return "", nil
	}

	if resp.StatusCode != http.StatusOK {
		return "", errors.New(resp.Status)
	}

	t, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	return string(t), nil
}

type transferKey struct {
	id     string
	peerId string
	up     bool
}

type transferredFile struct {
	id     string
	peerId string
	up     bool
	writer *writer[any]
}

var transferring sync.Map

func sendFiles(writer *writer[*clientMessage], dest string, files []string) {
	var wg sync.WaitGroup
	wg.Add(len(files))
	for _, filename := range files {
		go func(filename string) {
			err := sendFile(writer, dest, filename)
			if err != nil {
				log.Printf("Send %v: %v", filename, err)
			}
			wg.Done()
		}(filename)
	}
	wg.Wait()
}

func sendFile(writer *writer[*clientMessage], dest string, filename string) error {
	id := makeId()
	key := transferKey{
		id:     id,
		peerId: dest,
		up:     true,
	}
	tr := &transferredFile{
		id:     id,
		peerId: dest,
		up:     true,
		writer: newWriter[any](),
	}

	_, loaded := transferring.LoadOrStore(key, tr)
	if loaded {
		return errors.New("duplicate transfer")
	}

	defer transferring.Delete(tr.id)
	return sendLoop(writer, filename, tr)
}

func receiveFile(writer *writer[*clientMessage], source string, value map[string]any) error {
	id, ok := value["id"].(string)
	if !ok {
		return errors.New("Bad type for id")
	}
	key := transferKey{
		id:     id,
		peerId: source,
		up:     false,
	}
	tr := &transferredFile{
		id:     id,
		peerId: source,
		writer: newWriter[any](),
	}
	_, loaded := transferring.LoadOrStore(key, tr)
	if loaded {
		return errors.New("duplicate transfer")
	}
	defer transferring.Delete(tr.id)
	return receiveLoop(writer, tr, value)
}

func gotMessage(writer *writer[*clientMessage], source string, value map[string]any) error {
	tpe, ok := value["type"].(string)
	if !ok {
		return errors.New("Bad type for type")
	}
	id, ok := value["id"].(string)
	if !ok {
		return errors.New("Bad type for id")
	}
	up := false
	if tpe == "offer" || tpe == "reject" || tpe == "downice" {
		up = true
	}
	key := transferKey{
		id:     id,
		peerId: source,
		up:     up,
	}
	t, ok := transferring.Load(key)
	if !ok {
		return errors.New("unknown transfer")
	}
	tr := t.(*transferredFile)
	return tr.writer.write(value)
}

func receiveLoop(writer *writer[*clientMessage], tr *transferredFile, m map[string]any) error {
	write := func(v map[string]any) error {
		return writer.write(&clientMessage{
			Type:   "usermessage",
			Source: myId,
			Dest:   tr.peerId,
			Kind:   "filetransfer",
			Value:  v,
		})
	}

	abort := func(message string) {
		write(map[string]any{
			"type":  "reject",
			"id":    tr.id,
			"value": message,
		})
	}

	api := webrtc.NewAPI()
	var pc *webrtc.PeerConnection
	var dc *webrtc.DataChannel
	var file *os.File
	var size int64
	var count int64

	defer func() {
		if pc != nil {
			pc.Close()
			pc = nil
			dc = nil
		}
		if file != nil {
			file.Close()
			file = nil
		}
		close(tr.writer.done)
	}()

	ch := make(chan error, 8)

	for {
		tpe, ok := m["type"].(string)
		if !ok {
			abort("bad type for type")
			return errors.New("bad type for type")
		}
		switch tpe {
		case "invite":
			sz, ok := m["size"].(float64)
			if !ok {
				abort("bad type for size")
				return errors.New("bad type for size")
			}
			size = int64(sz)
			count = 0

			filename, ok := m["name"].(string)
			if !ok {
				abort("bad type for filename")
				return errors.New("bad type for filename")
			}
			if outfile == "" {
				filename = filepath.Base(filename)
			} else {
				fi, err := os.Stat(outfile)
				if err == nil && fi.IsDir() {
					filename = filepath.Join(
						outfile,
						filepath.Base(filename),
					)
				} else {
					filename = outfile
				}
			}
			var err error
			file, err = os.OpenFile(
				filename, os.O_WRONLY|os.O_CREATE|os.O_EXCL,
				0666,
			)
			if err != nil {
				abort(err.Error())
				return err
			}

			var conf webrtc.Configuration
			if rtcConfiguration != nil {
				conf = *rtcConfiguration
			}
			pc, err = api.NewPeerConnection(conf)
			if err != nil {
				abort(err.Error())
				return err
			}
			pc.OnICECandidate(func(c *webrtc.ICECandidate) {
				if c == nil {
					return
				}
				write(map[string]any{
					"type":      "downice",
					"id":        tr.id,
					"candidate": c.ToJSON(),
				})
			})
			dc, err = pc.CreateDataChannel("file", nil)
			if err != nil {
				abort(err.Error())
				return err
			}
			dc.OnMessage(func(msg webrtc.DataChannelMessage) {
				_, err := file.Write(msg.Data)
				if err != nil {
					abort(err.Error())
				}
				count += int64(len(msg.Data))
				if count >= size {
					if count == size {
						dc.SendText("done")
					} else {
						ch <- errors.New("extra data")
					}
				}
			})
			dc.OnError(func(err error) {
				ch <- err
			})
			dc.OnClose(func() {
				if count == size {
					ch <- nil
				} else {
					ch <- io.ErrUnexpectedEOF
				}
			})
			offer, err := pc.CreateOffer(nil)
			if err != nil {
				abort(err.Error())
				return err
			}
			err = pc.SetLocalDescription(offer)
			if err != nil {
				return err
			}
			write(map[string]any{
				"type": "offer",
				"id":   tr.id,
				"sdp":  pc.LocalDescription().SDP,
			})
		case "answer":
			sdp, ok := m["sdp"].(string)
			if !ok {
				abort("bad type for sdp")
				return errors.New("bad type for sdp")
			}
			err := pc.SetRemoteDescription(webrtc.SessionDescription{
				Type: webrtc.SDPTypeAnswer,
				SDP:  sdp,
			})
			if err != nil {
				abort(err.Error())
			}
		case "upice":
			if pc == nil {
				log.Println("got candidate for null PC")
				break
			}
			c, ok := m["candidate"].(map[string]any)
			if !ok {
				break
			}
			err := addIceCandidate(pc, c)
			if err != nil {
				log.Printf("AddIceCandidate: %v", err)
			}
		case "cancel":
			value, _ := m["value"].(string)
			if value != "" {
				return errors.New("canceled: " + value)
			} else {
				return errors.New("canceled")
			}
		default:
			log.Println("unexpected", m["type"])
		}

		select {
		case v, ok := <-tr.writer.ch:
			if !ok {
				return nil
			}
			switch v := v.(type) {
			case nil:
				return nil
			case map[string]any:
				m = v
			default:
				log.Println("Unexpected message", v)
			}
		case err := <-ch:
			if err != nil {
				abort(err.Error())
			}
			return err
		}
	}
}

func sendLoop(writer *writer[*clientMessage], filename string, tr *transferredFile) error {
	write := func(v map[string]any) error {
		return writer.write(&clientMessage{
			Type:   "usermessage",
			Source: myId,
			Dest:   tr.peerId,
			Kind:   "filetransfer",
			Value:  v,
		})
	}

	abort := func(message string) {
		write(map[string]any{
			"type":  "cancel",
			"id":    tr.id,
			"value": message,
		})
	}

	file, err := os.Open(filename)
	if err != nil {
		return err
	}

	fi, err := file.Stat()
	if err != nil {
		return err
	}

	err = write(map[string]any{
		"type": "invite",
		"id":   tr.id,
		"name": filepath.Base(filename),
		"size": fi.Size(),
	})
	if err != nil {
		return err
	}

	api := webrtc.NewAPI()
	var pc *webrtc.PeerConnection

	defer func() {
		if pc != nil {
			pc.Close()
			pc = nil
		}
	}()

	ch := make(chan error, 8)

	for {
		var m map[string]any
		select {
		case v, ok := <-tr.writer.ch:
			if !ok {
				return nil
			}
			switch v := v.(type) {
			case nil:
				return nil
			case map[string]any:
				m = v
			default:
				log.Println("Unexpected message", v)
			}
		case err := <-ch:
			if err != nil {
				abort(err.Error())
			}
			return err
		}

		tpe, ok := m["type"].(string)
		if !ok {
			abort("bad type for type")
			return errors.New("bad type for type")
		}
		switch tpe {
		case "offer":
			if pc != nil {
				log.Println("Duplicate offer")
				break
			}
			sdp, ok := m["sdp"].(string)
			if !ok {
				abort("bad type for sdp")
				return errors.New("bad type for sdp")
			}
			var conf webrtc.Configuration
			if rtcConfiguration != nil {
				conf = *rtcConfiguration
			}
			pc, err = api.NewPeerConnection(conf)
			if err != nil {
				abort(err.Error())
				return err
			}

			pc.OnDataChannel(func(dc *webrtc.DataChannel) {
				dc.OnClose(func() {
					ch <- io.ErrUnexpectedEOF
				})
				dc.OnMessage(func(msg webrtc.DataChannelMessage) {
					done := []byte("done")
					if bytes.Equal(msg.Data, done) {
						ch <- nil
					} else {
						ch <- errors.New(
							"unexpected message",
						)
					}
				})
				dc.OnError(func(err error) {
					ch <- err
				})
				go func() {
					buf := make([]byte, 16384)
					for {
						n, err := file.Read(buf)
						if n > 0 {
							err2 := dc.Send(buf[:n])
							if err != nil {
								log.Println(err2)
							}
						}
						if err != nil {
							break
						}
					}
				}()
			})

			pc.OnICECandidate(func(c *webrtc.ICECandidate) {
				if c == nil {
					return
				}
				write(map[string]any{
					"type":      "upice",
					"id":        tr.id,
					"candidate": c.ToJSON(),
				})
			})

			err = pc.SetRemoteDescription(webrtc.SessionDescription{
				Type: webrtc.SDPTypeOffer,
				SDP:  sdp,
			})
			if err != nil {
				abort(err.Error())
				return err
			}

			answer, err := pc.CreateAnswer(nil)
			if err != nil {
				abort(err.Error())
				return err
			}

			err = pc.SetLocalDescription(answer)
			if err != nil {
				abort(err.Error())
				return err
			}
			write(map[string]any{
				"type": "answer",
				"id":   tr.id,
				"sdp":  pc.LocalDescription().SDP,
			})
		case "downice":
			if pc == nil {
				log.Println("got candidate for null PC")
				break
			}
			c, ok := m["candidate"].(map[string]any)
			if !ok {
				break
			}
			err := addIceCandidate(pc, c)
			if err != nil {
				log.Printf("AddIceCandidate: %v", err)
			}
		case "reject":
			value, _ := m["value"].(string)
			if value != "" {
				return errors.New("rejected: " + value)
			} else {
				return errors.New("rejected")
			}
		default:
			log.Println("unexpected", m["type"])
		}
	}
}

func addIceCandidate(pc *webrtc.PeerConnection, candidate map[string]any) error {
	j, err := json.Marshal(candidate)
	if err != nil {
		return err
	}
	var init webrtc.ICECandidateInit
	err = json.Unmarshal(j, &init)
	if err != nil {
		return err
	}
	return pc.AddICECandidate(init)
}

func abortTransfers(id string) {
	transferring.Range(func(key any, value any) bool {
		t := value.(*transferredFile)
		if id != "" && id != t.peerId {
			return true
		}
		t.writer.write(nil)
		return true
	})
}
