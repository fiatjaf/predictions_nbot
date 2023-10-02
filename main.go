package main

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/kelseyhightower/envconfig"
	"github.com/nbd-wtf/go-nostr"
	"github.com/nbd-wtf/opentimestamps"
)

var s Settings

type Settings struct {
	SecretKey string `envconfig:"SECRET_KEY" required:"true"`
	Calendar  string `envconfig:"CALENDAR" default:"https://alice.btc.calendar.opentimestamps.org/"`
	Esplora   string `envconfig:"ESPLORA" default:"https://blockstream.info/api"`
}

const (
	FILES_SUBDIR = "data/"
	PREFIX_OTS   = "time-"
	SUFFIX_OTS   = ".ots"
	PREFIX_RELAY = "relay-"
	SUFFIX_RELAY = ".txt"
	PREFIX_EVENT = "event-"
	SUFFIX_EVENT = ".json"
)

func main() {
	if err := envconfig.Process("", &s); err != nil {
		log.Fatalf("failed to read from env: %s", err)
		return
	}

	os.Mkdir("data", 0755)

	ctx := context.Background()
	pool := nostr.NewSimplePool(ctx)

	// every hour, try to upgrade our pending attestations
	go func() {
		_, err := os.ReadDir(FILES_SUBDIR)
		if err != nil {
			panic(err)
		}

		time.Sleep(5 * time.Second)

		for {
			fmt.Println("trying to publish events for finalized timestamps")

			files, err := os.ReadDir(FILES_SUBDIR)
			if err != nil {
				fmt.Println("error reading directory:", err)
				continue
			}

			var blockHeight string
			var blockHash string

			if resp, err := http.Get(s.Esplora + "/blocks/tip/height"); err != nil {
				fmt.Println("error getting block height:", err)
				continue
			} else {
				b, err := io.ReadAll(resp.Body)
				resp.Body.Close()
				if err != nil {
					fmt.Println("error reading block height response:", err)
					continue
				}
				blockHeight = string(b)
			}

			if resp, err := http.Get(s.Esplora + "/blocks/tip/hash"); err != nil {
				fmt.Println("error getting block height:", err)
				continue
			} else {
				b, err := io.ReadAll(resp.Body)
				resp.Body.Close()
				if err != nil {
					fmt.Println("error reading block height response:", err)
					continue
				}
				blockHash = string(b)
			}

			for _, file := range files {
				filename := file.Name()
				fmt.Println("trying file", filename, strings.HasPrefix(filename, PREFIX_OTS), PREFIX_OTS, strings.HasSuffix(filename, SUFFIX_OTS))
				if strings.HasPrefix(filename, PREFIX_OTS) && strings.HasSuffix(filename, SUFFIX_OTS) {
					id := filename[len(PREFIX_OTS) : len(filename)-len(SUFFIX_OTS)]
					if len(id) != 64 {
						fmt.Println("  id is invalid:", id)
						continue
					}
					fmt.Println("  trying to upgrade " + id)

					// read ots from file
					data, err := os.ReadFile(FILES_SUBDIR + PREFIX_OTS + id + SUFFIX_OTS)
					if err != nil {
						fmt.Println("    error reading:", err)
						continue
					}
					ots, err := opentimestamps.ReadFromFile(data)
					if err != nil {
						fmt.Println("    error parsing:", err)
						continue
					}

					// read event from file
					var event nostr.Event
					if eventb, err := os.ReadFile(FILES_SUBDIR + PREFIX_EVENT + id + SUFFIX_EVENT); err != nil {
						fmt.Println("    error reading event:", err)
						continue
					} else if err := json.Unmarshal(eventb, &event); err != nil {
						fmt.Println("    error parsing event:", err)
						continue
					}

					// read event relays from file
					eventRelay := ""
					if relayb, err := os.ReadFile(FILES_SUBDIR + PREFIX_RELAY + id + SUFFIX_RELAY); err != nil {
						fmt.Println("    error reading event relays:", err)
						continue
					} else {
						eventRelay = strings.TrimSpace(string(relayb))
					}

					// try to upgrade now
					for _, seq := range ots.Sequences {
						ictx, cancel := context.WithTimeout(ctx, time.Minute)
						newSeq, err := seq.Upgrade(ictx, ots.Digest)
						cancel()
						if err != nil {
							fmt.Println("    failed:", err)
							continue
						}
						fmt.Println("    upgraded", newSeq[len(newSeq)-1].Attestation.BitcoinBlockHeight)

						file := opentimestamps.File{Digest: ots.Digest, Sequences: []opentimestamps.Sequence{newSeq}}
						event := nostr.Event{
							CreatedAt: nostr.Now(),
							Kind:      1040,
							Content:   base64.StdEncoding.EncodeToString(file.SerializeToFile()),
							Tags: nostr.Tags{
								nostr.Tag{"e", event.ID, eventRelay},
								nostr.Tag{"p", event.PubKey},
								nostr.Tag{"block", blockHeight, blockHash},
							},
						}

						relay, err := pool.EnsureRelay(eventRelay)
						if err != nil {
							fmt.Println("    failed to get relay", eventRelay)
							continue
						}

						if err := event.Sign(s.SecretKey); err != nil {
							panic(fmt.Errorf("    failed to sign: %w", err))
						}

						fmt.Println("    publishing", event)

						ictx, cancel = context.WithTimeout(ctx, time.Minute)
						status, err := relay.Publish(ictx, event)
						cancel()

						if err == nil && status == nostr.PublishStatusSucceeded {
							fmt.Println("    published to", relay.URL)
							os.Remove(FILES_SUBDIR + PREFIX_OTS + id + SUFFIX_OTS)
							os.Remove(FILES_SUBDIR + PREFIX_RELAY + id + SUFFIX_RELAY)
							os.Remove(FILES_SUBDIR + PREFIX_EVENT + id + SUFFIX_EVENT)
						}

						break
					}
				}
			}

			time.Sleep(time.Hour)
		}
	}()

	// listen for new events and timestamp them
	for {
		events := pool.SubMany(ctx, []string{
			"wss://nostr.mom",
			"wss://nostr.wine",
			"wss://public.relaying.io",
			"wss://nostr-pub.wellorder.net",
		}, nostr.Filters{
			{
				Limit: 1,
				Tags:  nostr.TagMap{"t": []string{"prediction"}},
			},
		})

		for event := range events {
			fmt.Println("stamping event", event.Event)

			if _, err := os.Stat(PREFIX_OTS + event.ID + SUFFIX_OTS); err == nil {
				fmt.Println("  stamp file already exists")
				continue
			}

			// saving event and relay file
			if err := os.WriteFile(FILES_SUBDIR+PREFIX_EVENT+event.ID+SUFFIX_EVENT, []byte(event.String()), 0644); err != nil {
				fmt.Println("  failed to save event file", event.ID, "->", err)
				continue
			}
			if err := os.WriteFile(FILES_SUBDIR+PREFIX_RELAY+event.ID+SUFFIX_RELAY, []byte(event.Relay.URL), 0644); err != nil {
				fmt.Println("  failed to save event relay file", event.ID, "->", err)
				continue
			}

			// stamping on calendar server and saving ots file
			id, _ := hex.DecodeString(event.ID)
			var digest [32]byte
			copy(digest[:], id)
			seq, err := opentimestamps.Stamp(ctx, s.Calendar, digest)
			if err != nil {
				fmt.Println(" failed to stamp", event.ID, "->", err)
				continue
			}

			file := opentimestamps.File{Digest: id, Sequences: []opentimestamps.Sequence{seq}}
			if err := os.WriteFile(FILES_SUBDIR+PREFIX_OTS+event.ID+SUFFIX_OTS, file.SerializeToFile(), 0644); err != nil {
				fmt.Println("  failed to save stamp file", event.ID, "->", err)
				continue
			}

			fmt.Println("  saved stamp file", event.ID)
		}

		fmt.Println("### lost connection to all relays, will start again after 5 minutes")
		time.Sleep(5 * time.Minute)
	}
}
