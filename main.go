package main

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"runtime/debug"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/comerc/shunt/config"
	"github.com/dgraph-io/badger"
	"github.com/joho/godotenv"
	"github.com/zelenin/go-tdlib/client"
)

var (
	configData  *config.Config
	tdlibClient *client.Client
	// configMu      sync.Mutex
	badgerDB *badger.DB
)

func main() {
	log.SetFlags(log.LUTC | log.Ldate | log.Ltime | log.Lshortfile)
	var err error

	if err = godotenv.Load(); err != nil {
		log.Fatalf("Error loading .env file")
	}

	path := filepath.Join(".", ".tdata")
	if _, err = os.Stat(path); os.IsNotExist(err) {
		os.Mkdir(path, os.ModePerm)
	}

	{
		path := filepath.Join(path, "badger")
		if _, err = os.Stat(path); os.IsNotExist(err) {
			os.Mkdir(path, os.ModePerm)
		}
		badgerDB, err = badger.Open(badger.DefaultOptions(path))
		if err != nil {
			log.Fatal(err)
		}
	}
	defer badgerDB.Close()

	go func() {
		ticker := time.NewTicker(5 * time.Minute)
		defer ticker.Stop()
		for range ticker.C {
		again:
			err := badgerDB.RunValueLogGC(0.7)
			if err == nil {
				goto again
			}
		}
	}()

	var (
		apiId    = os.Getenv("SHUNT_API_ID")
		apiHash  = os.Getenv("SHUNT_API_HASH")
		botToken = os.Getenv("SHUNT_SECRET")
	)

	go config.Watch(func() {
		tmp, err := config.Load()
		if err != nil {
			log.Printf("Can't initialise config: %s", err)
			return
		}
		// configMu.Lock()
		// defer configMu.Unlock()
		configData = tmp
		// TODO: надо дожидаться инициализации конфига при старте
	})

	// or bot authorizer
	authorizer := client.BotAuthorizer(botToken)

	authorizer.TdlibParameters <- &client.TdlibParameters{
		UseTestDc:              false,
		DatabaseDirectory:      filepath.Join(path, "db"),
		FilesDirectory:         filepath.Join(path, "files"),
		UseFileDatabase:        false,
		UseChatInfoDatabase:    false,
		UseMessageDatabase:     true,
		UseSecretChats:         false,
		ApiId:                  int32(convertToInt(apiId)),
		ApiHash:                apiHash,
		SystemLanguageCode:     "en",
		DeviceModel:            "Server",
		SystemVersion:          "1.0.0",
		ApplicationVersion:     "1.0.0",
		EnableStorageOptimizer: true,
		IgnoreFileNames:        false,
	}

	logStream := func(tdlibClient *client.Client) {
		tdlibClient.SetLogStream(&client.SetLogStreamRequest{
			LogStream: &client.LogStreamFile{
				Path:           filepath.Join(path, ".log"),
				MaxFileSize:    10485760,
				RedirectStderr: true,
			},
		})
	}

	logVerbosity := func(tdlibClient *client.Client) {
		tdlibClient.SetLogVerbosityLevel(&client.SetLogVerbosityLevelRequest{
			NewVerbosityLevel: 1,
		})
	}

	// client.WithProxy(&client.AddProxyRequest{})

	tdlibClient, err = client.NewClient(authorizer, logStream, logVerbosity)
	if err != nil {
		log.Fatalf("NewClient error: %s", err)
	}
	defer tdlibClient.Stop()

	log.Print("Start...")

	if optionValue, err := tdlibClient.GetOption(&client.GetOptionRequest{
		Name: "version",
	}); err != nil {
		log.Fatalf("GetOption error: %s", err)
	} else {
		log.Printf("TDLib version: %s", optionValue.(*client.OptionValueString).Value)
	}

	if me, err := tdlibClient.GetMe(); err != nil {
		log.Fatalf("GetMe error: %s", err)
	} else {
		log.Printf("Me: %s %s [@%s]", me.FirstName, me.LastName, me.Username)
	}

	listener := tdlibClient.GetListener()
	defer listener.Close()

	// Handle Ctrl+C
	ch := make(chan os.Signal, 2)
	signal.Notify(ch, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-ch
		log.Print("Stop...")
		os.Exit(1)
	}()

	defer handlePanic()

	for update := range listener.Updates {
		if update.GetClass() == client.ClassUpdate {
			log.Printf("%#v", update)
			if updateNewCallbackQuery, ok := update.(*client.UpdateNewCallbackQuery); ok {
				if configData.Main != updateNewCallbackQuery.ChatId {
					continue
				}
				errorCode := func() int {
					data := ""
					if callbackQueryPayloadData, ok := updateNewCallbackQuery.Payload.(*client.CallbackQueryPayloadData); ok {
						data = string(callbackQueryPayloadData.Data)
					} else {
						return 1000
					}
					if data == "" {
						log.Print("CallbackQueryPayloadData: Data is empty")
						return 1001
					}
					a := strings.Split(data, "|")
					if len(a) != 3 {
						log.Print("CallbackQueryPayloadData: Invalid Data - len() ")
						return 1002
					}
					command := a[0]
					if !contains([]string{"OK", "CANCEL"}, command) {
						log.Print("CallbackQueryPayloadData: Invalid Data - command")
						return 1003
					}
					srcId := int64(convertToInt(a[1]))
					if srcId == 0 {
						log.Print("CallbackQueryPayloadData: Invalid Data - srcId")
						return 1004
					}
					lastUrl := a[2]
					var messageIds []int64
					messageLinkInfo, err := tdlibClient.GetMessageLinkInfo(&client.GetMessageLinkInfoRequest{
						Url: lastUrl,
					})
					if err != nil {
						log.Print(err)
						return 1005
					}
					if messageLinkInfo.Message == nil {
						log.Print("GetMessageLinkInfo(): messageLinkInfo.Message is empty")
						return 1006
					}
					if messageLinkInfo.ForAlbum {
						mediaAlbumId := getMediaAlbumIdByMessageId(messageLinkInfo.Message.Id)
						if mediaAlbumId == 0 {
							log.Print("mediaAlbumId is empty")
							return 1007
						}
						messageIds = getMessageIdsByMediaAlbumId(mediaAlbumId)
						if len(messageIds) == 0 {
							log.Print("messageIds is empty")
							return 1008
						}
					} else {
						messageIds = []int64{messageLinkInfo.Message.Id}
					}
					for _, forward := range configData.Forwards {
						if forward.From == messageLinkInfo.ChatId {
							messages, err := tdlibClient.ForwardMessages(&client.ForwardMessagesRequest{
								ChatId: func() int64 {
									if command == "OK" {
										return forward.CopyTo
									}
									return configData.Cancel
								}(),
								FromChatId: messageLinkInfo.ChatId,
								MessageIds: messageIds,
							})
							if err != nil {
								log.Print("ForwardMessages() ", err)
								return 1009
							} else if len(messages.Messages) != int(messages.TotalCount) || messages.TotalCount == 0 {
								log.Print("ForwardMessages(): invalid TotalCount")
								return 1010
							}
							messageIds = append(messageIds, updateNewCallbackQuery.MessageId)
							if _, err := tdlibClient.DeleteMessages(&client.DeleteMessagesRequest{
								ChatId:     updateNewCallbackQuery.ChatId,
								MessageIds: messageIds,
							}); err != nil {
								log.Print(err)
								return 1011
							}

						}
					}
					return 0
				}()
				_, err := tdlibClient.AnswerCallbackQuery(&client.AnswerCallbackQueryRequest{
					CallbackQueryId: updateNewCallbackQuery.Id,
					Text: func() string {
						if errorCode > 0 {
							return fmt.Sprintf("Error! Code %d", errorCode)
						}
						return ""
					}(),
				})
				if err != nil {
					log.Print(err)
				}
			} else if updateNewMessage, ok := update.(*client.UpdateNewMessage); ok {
				src := updateNewMessage.Message
				if src.IsOutgoing {
					continue
				}
				isFrom := false
				for _, forward := range configData.Forwards {
					if forward.From == src.ChatId {
						isFrom = true
						break
					}
				}
				if isFrom {
					mediaAlbumId := int64(src.MediaAlbumId)
					if mediaAlbumId != 0 {
						setMediaAlbumIdByMessageId(src.Id, mediaAlbumId)
						messageIds := getMessageIdsByMediaAlbumId(mediaAlbumId)
						if len(messageIds) > 0 {
							messageIds = append(messageIds, src.Id)
							setMessageIdsByMediaAlbumId(mediaAlbumId, messageIds)
							continue
						}
						messageIds = []int64{src.Id}
						setMessageIdsByMediaAlbumId(mediaAlbumId, messageIds)
					}
					continue
				}
				if configData.Main == src.ChatId {
					// TODO: https://github.com/tdlib/td/issues/1649
					messageLink, err := tdlibClient.GetMessageLink(&client.GetMessageLinkRequest{
						ChatId:    src.ChatId,
						MessageId: src.Id,
						ForAlbum:  src.MediaAlbumId != 0,
					})
					if err != nil {
						log.Print(err)
						continue
					}
					formattedText, err := tdlibClient.ParseTextEntities(&client.ParseTextEntitiesRequest{
						Text: fmt.Sprintf("[⤴️](%s)", messageLink.Link),
						ParseMode: &client.TextParseModeMarkdown{
							Version: 2,
						},
					})
					if err != nil {
						log.Print("ParseTextEntities() ", err)
						continue
					}
					lastUrl := ""
					if formattedText := getFormattedText(src.Content); formattedText != nil {
						l := len(formattedText.Entities)
						if l > 0 {
							lastEntity := formattedText.Entities[l-1]
							if url, ok := lastEntity.Type.(*client.TextEntityTypeTextUrl); ok {
								lastUrl = url.Url
							}
						}
					}
					if lastUrl == "" {
						log.Print("MessageText: lastUrl is empty")
						continue
					}
					if _, err := tdlibClient.SendMessage(&client.SendMessageRequest{
						ChatId: src.ChatId,
						InputMessageContent: &client.InputMessageText{
							Text:                  formattedText,
							DisableWebPagePreview: true,
							ClearDraft:            true,
						},
						Options: &client.MessageSendOptions{
							DisableNotification: true,
						},
						ReplyMarkup: func() client.ReplyMarkup {
							Rows := make([][]*client.InlineKeyboardButton, 0)
							Btns := make([]*client.InlineKeyboardButton, 0)
							Btns = append(Btns, &client.InlineKeyboardButton{
								Text: "✅ Yes!",
								Type: &client.InlineKeyboardButtonTypeCallback{
									Data: []byte(fmt.Sprintf("OK|%d|%s", src.Id, lastUrl)),
								},
							})
							Btns = append(Btns, &client.InlineKeyboardButton{
								Text: "🛑 Stop",
								Type: &client.InlineKeyboardButtonTypeCallback{
									Data: []byte(fmt.Sprintf("CANCEL|%d|", src.Id)),
								},
							})
							Rows = append(Rows, Btns)
							return &client.ReplyMarkupInlineKeyboard{Rows: Rows}
						}(),
					}); err != nil {
						log.Print("SendMessage() ", err)
					}
				}
			}
		}
	}
}

func convertToInt(s string) int {
	i, err := strconv.Atoi(s)
	if err != nil {
		log.Print("convertToInt() ", err)
		return 0
	}
	return int(i)
}

func contains(a []string, s string) bool {
	for _, t := range a {
		if t == s {
			return true
		}
	}
	return false
}

// func containsInt64(a []int64, e int64) bool {
// 	for _, t := range a {
// 		if t == e {
// 			return true
// 		}
// 	}
// 	return false
// }

func handlePanic() {
	if err := recover(); err != nil {
		log.Printf("Panic...\n%s\n\n%s", err, debug.Stack())
		os.Exit(1)
	}
}

// **** db routines

// func uint64ToBytes(i uint64) []byte {
// 	var buf [8]byte
// 	binary.BigEndian.PutUint64(buf[:], i)
// 	return buf[:]
// }

// func bytesToUint64(b []byte) uint64 {
// 	return binary.BigEndian.Uint64(b)
// }

// func incrementByDB(key []byte) []byte {
// 	// Merge function to add two uint64 numbers
// 	add := func(existing, new []byte) []byte {
// 		return uint64ToBytes(bytesToUint64(existing) + bytesToUint64(new))
// 	}
// 	m := badgerDB.GetMergeOperator(key, add, 200*time.Millisecond)
// 	defer m.Stop()
// 	m.Add(uint64ToBytes(1))
// 	result, _ := m.Get()
// 	return result
// }

func getByDB(key []byte) []byte {
	var (
		err error
		val []byte
	)
	err = badgerDB.View(func(txn *badger.Txn) error {
		var item *badger.Item
		item, err = txn.Get(key)
		if err != nil {
			return err
		}
		val, err = item.ValueCopy(nil)
		if err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		log.Printf("getByDB() key: %s err: %s", string(key), err)
		// } else {
		// log.Printf("getByDB() key: %s val: %s", string(key), string(val))
	}
	return val
}

func setByDB(key []byte, val []byte) {
	err := badgerDB.Update(func(txn *badger.Txn) error {
		err := txn.Set(key, val)
		return err
	})
	if err != nil {
		log.Printf("setByDB() key: %s err: %s ", string(key), err)
		// } else {
		// log.Printf("setByDB() key: %s val: %s", string(key), string(val))
	}
}

// func deleteByDB(key []byte) {
// 	err := badgerDB.Update(func(txn *badger.Txn) error {
// 		return txn.Delete(key)
// 	})
// 	if err != nil {
// 		log.Print(err)
// 	}
// }

// func distinct(a []string) []string {
// 	set := make(map[string]struct{})
// 	for _, val := range a {
// 		set[val] = struct{}{}
// 	}
// 	result := make([]string, 0, len(set))
// 	for key := range set {
// 		result = append(result, key)
// 	}
// 	return result
// }

// func strLen(s string) int {
// 	return len(utf16.Encode([]rune(s)))
// }

// func escapeAll(s string) string {
// 	// эскейпит все символы: которые нужны для markdown-разметки
// 	a := []string{
// 		"_",
// 		"*",
// 		`\[`,
// 		`\]`,
// 		"(",
// 		")",
// 		"~",
// 		"`",
// 		">",
// 		"#",
// 		"+",
// 		`\-`,
// 		"=",
// 		"|",
// 		"{",
// 		"}",
// 		".",
// 		"!",
// 	}
// 	re := regexp.MustCompile("[" + strings.Join(a, "|") + "]")
// 	return re.ReplaceAllString(s, `\$0`)
// }

// func getRand(min, max int) int {
// 	rand.Seed(time.Now().UnixNano())
// 	return rand.Intn(max-min+1) + min
// }

func getFormattedText(messageContent client.MessageContent) *client.FormattedText {
	switch content := messageContent.(type) {
	case *client.MessageText:
		return content.Text
	case *client.MessagePhoto:
		return content.Caption
	case *client.MessageAnimation:
		return content.Caption
	case *client.MessageAudio:
		return content.Caption
	case *client.MessageDocument:
		return content.Caption
	case *client.MessageVideo:
		return content.Caption
	case *client.MessageVoiceNote:
		return content.Caption
	}
	return nil
}

const messageIdsByMediaAlbumIdPrefix = "msg-ma"

func setMessageIdsByMediaAlbumId(mediaAlbumId int64, messageIds []int64) {
	key := []byte(fmt.Sprintf("%s:%d", messageIdsByMediaAlbumIdPrefix, mediaAlbumId))
	val, err := json.Marshal(messageIds)
	if err != nil {
		log.Print(err)
		return
	}
	setByDB(key, val)
}

func getMessageIdsByMediaAlbumId(mediaAlbumId int64) []int64 {
	key := []byte(fmt.Sprintf("%s:%d", messageIdsByMediaAlbumIdPrefix, mediaAlbumId))
	val := getByDB(key)
	if val == nil {
		return nil
	}
	var result []int64
	if err := json.Unmarshal(val, &result); err != nil {
		log.Print(err)
		return nil
	}
	return result
}

const mediaAlbumIdByMessageIdPrefix = "ma-msg"

func setMediaAlbumIdByMessageId(messageId int64, mediaAlbumId int64) {
	key := []byte(fmt.Sprintf("%s:%d", mediaAlbumIdByMessageIdPrefix, messageId))
	val := []byte(fmt.Sprintf("%d", mediaAlbumId))
	setByDB(key, val)
}

func getMediaAlbumIdByMessageId(messageId int64) int64 {
	key := []byte(fmt.Sprintf("%s:%d", mediaAlbumIdByMessageIdPrefix, messageId))
	val := getByDB(key)
	if val == nil {
		return 0
	}
	return int64(convertToInt(string(val)))
}
