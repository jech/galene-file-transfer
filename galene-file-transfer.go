package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sync"
	"syscall"

	"github.com/gorilla/websocket"
	"github.com/jech/gclient"
	"github.com/pion/webrtc/v4"
)

var outfile string
var rtcConfiguration *webrtc.Configuration
var insecure bool

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
	flag.BoolVar(&gclient.Debug, "debug", false,
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

	client := gclient.NewClient()

	if insecure {
		t := http.DefaultTransport.(*http.Transport).Clone()
		t.TLSClientConfig = &tls.Config{InsecureSkipVerify: true}
		client.SetHTTPClient(&http.Client{
			Transport: t,
		})

		d := *websocket.DefaultDialer
		d.TLSClientConfig = &tls.Config{InsecureSkipVerify: true}
		client.SetDialer(&d)
	}

	err := client.Connect(context.Background(), flag.Arg(0))
	if err != nil {
		log.Fatalf("Connect: %v", err)
	}

	done := make(chan struct{})
	terminate := make(chan os.Signal, 1)
	signal.Notify(terminate, syscall.SIGINT, syscall.SIGTERM)

	err = client.Join(
		context.Background(), flag.Arg(0), username, password,
	)
	if err != nil {
		log.Fatalf("Join: %v", err)
	}

	sent := false
outer:
	for {
		select {
		case <-terminate:
			break outer
		case <-done:
			break outer
		case e := <-client.EventCh:
			switch e := e.(type) {
			case gclient.JoinedEvent:
				if e.Kind == "failed" {
					log.Printf("Join failed: %v", e.Value)
					break outer
				}
			case gclient.UserMessageEvent:
				if e.Kind == "error" || e.Kind == "warning" {
					log.Printf("%v: %v", e.Kind, e.Value)
					break
				}
				if e.Kind != "filetransfer" {
					log.Println("Unexpected usermessage",
						e.Kind)
					break
				}
				value, ok := e.Value.(map[string]any)
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
						v := map[string]any{
							"type": "cancel",
							"id":   value["id"],
							"message": "this peer " +
								"does not" +
								"receive files",
						}
						client.UserMessage(
							e.Source,
							"filetransfer",
							v,
						)
						continue outer
					}
					go func() {
						err := receiveFile(
							client, e.Source, value,
						)
						if err != nil {
							log.Printf("Receive: %v", err)
						}
						if !persist {
							close(done)
						}
					}()
				} else {
					err := gotMessage(client, e.Source, value)
					if err != nil {
						log.Printf("File transfer: %v", err)
						break outer
					}
				}
			case gclient.UserEvent:
				switch e.Kind {
				case "delete":
					abortTransfers(e.Id)
				case "add", "change":
					if e.Id == client.Id {
						continue outer
					}
					if sending && !sent &&
						e.Username == toUsername {
						sent = true
						go func(filenames []string) {
							sendFiles(
								client, e.Id,
								filenames,
							)
							if !persist {
								close(done)
							}
						}(flag.Args()[1:])
					}
				}
			case gclient.DownConnEvent:
				client.Abort(e.Id)
			case error:
				log.Printf("Protocol error: %v", e)
				break outer
			}
		}
	}

	abortTransfers("")
	client.Close()
}

type transferKey struct {
	id     string
	peerId string
}

type transferredFile struct {
	id      string
	peerId  string
	up      bool
	writeCh chan map[string]any
}

var transferring sync.Map

func sendFiles(client *gclient.Client, dest string, files []string) {
	var wg sync.WaitGroup
	wg.Add(len(files))
	for _, filename := range files {
		go func(filename string) {
			err := sendFile(client, dest, filename)
			if err != nil {
				log.Printf("Send %v: %v", filename, err)
			}
			wg.Done()
		}(filename)
	}
	wg.Wait()
}

func sendFile(client *gclient.Client, dest string, filename string) error {
	id := gclient.MakeId()
	key := transferKey{
		id:     id,
		peerId: dest,
	}
	tr := &transferredFile{
		id:      id,
		peerId:  dest,
		up:      true,
		writeCh: make(chan map[string]any, 8),
	}

	_, loaded := transferring.LoadOrStore(key, tr)
	if loaded {
		return errors.New("duplicate transfer")
	}

	defer transferring.Delete(tr.id)
	return sendLoop(client, filename, tr)
}

func receiveFile(client *gclient.Client, source string, value map[string]any) error {
	id, ok := value["id"].(string)
	if !ok {
		return errors.New("Bad type for id")
	}
	key := transferKey{
		id:     id,
		peerId: source,
	}
	tr := &transferredFile{
		id:      id,
		peerId:  source,
		writeCh: make(chan map[string]any, 8),
	}
	_, loaded := transferring.LoadOrStore(key, tr)
	if loaded {
		return errors.New("duplicate transfer")
	}
	defer transferring.Delete(key)
	return receiveLoop(client, tr, value)
}

func gotMessage(client *gclient.Client, source string, value map[string]any) error {
	id, ok := value["id"].(string)
	if !ok {
		return errors.New("Bad type for id")
	}
	key := transferKey{
		id:     id,
		peerId: source,
	}
	t, ok := transferring.Load(key)
	if !ok {
		return errors.New("unknown transfer")
	}
	tr := t.(*transferredFile)
	tr.writeCh <- value
	return nil
}

func contains(s []any, v string) bool {
	for i := range s {
		w, ok := s[i].(string)
		if !ok {
			continue
		}
		if v == w {
			return true
		}
	}
	return false
}

func receiveLoop(client *gclient.Client, tr *transferredFile, m map[string]any) error {
	write := func(v map[string]any) error {
		return client.UserMessage(
			tr.peerId,
			"filetransfer",
			v,
		)
	}

	abort := func(message string) {
		write(map[string]any{
			"type":    "cancel",
			"id":      tr.id,
			"message": message,
		})
	}

	api := webrtc.NewAPI()
	var protocolVersion string
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
		close(tr.writeCh)
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
			v, ok := m["version"].([]any)
			if ok && contains(v, "1") {
				protocolVersion = "1"
			} else {
				abort(fmt.Sprintf(
					"unknown protocol version %v", v,
				))
				return fmt.Errorf(
					"unknown protocol version %v", v,
				)
			}
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
					"type":      "ice",
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
				"type":    "offer",
				"version": []string{protocolVersion},
				"id":      tr.id,
				"sdp":     pc.LocalDescription().SDP,
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
		case "ice":
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
			value, _ := m["message"].(string)
			if value != "" {
				return errors.New("cancelled: " + value)
			} else {
				return errors.New("cancelled")
			}
		default:
			log.Println("unexpected", m["type"])
		}

		select {
		case v, ok := <-tr.writeCh:
			if !ok {
				return nil
			}
			if v == nil {
				return nil
			}
			m = v
		case err := <-ch:
			if err != nil {
				abort(err.Error())
			}
			return err
		}
	}
}

func sendLoop(client *gclient.Client, filename string, tr *transferredFile) error {
	write := func(v map[string]any) error {
		return client.UserMessage(
			tr.peerId,
			"filetransfer",
			v,
		)
	}

	abort := func(message string) {
		write(map[string]any{
			"type":    "cancel",
			"id":      tr.id,
			"message": message,
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
		"type":    "invite",
		"version": []string{"1"},
		"id":      tr.id,
		"name":    filepath.Base(filename),
		"size":    fi.Size(),
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
		case v, ok := <-tr.writeCh:
			if !ok {
				return nil
			}
			if v == nil {
				return nil
			}
			m = v
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
			v, ok := m["version"].([]any)
			if !ok || !contains(v, "1") {
				abort(fmt.Sprintf(
					"unknown protocol version %v", v,
				))
				return fmt.Errorf(
					"unknown protocol version %v", v,
				)
			}

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
					"type":      "ice",
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
		case "ice":
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
			value, _ := m["message"].(string)
			if value != "" {
				return errors.New("cancelled: " + value)
			} else {
				return errors.New("cancelled")
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
		t.writeCh <- nil
		return true
	})
}
