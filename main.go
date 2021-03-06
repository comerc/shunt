package main

// TODO: отключение модерации, прямая пересылка из From в To (т.к. надо накапливать ссылочную БД) + AutoAnswers в budva32

// TODO: функционал для модерирования редактирования (можно хранить историю через ReplyToMessageId?); учитывать, что может быть несколько копий в dst - нужно везде внести изменения из dst
// копирование в DST отредактированных сообщений (Forward.EditHistory) после модерации, как редактирование первого в цепочке ответов

// TODO: строковые константы вместо числовых кодов ошибок

import (
	"container/list"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"runtime/debug"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unicode/utf16"

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
		// TODO: проверка, что Main (ну и Trash) не назначен в Forwards
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

	go runQueue()

	for update := range listener.Updates {
		if update.GetClass() == client.ClassUpdate {
			// log.Printf("%#v", update)
			switch updateType := update.(type) {
			case *client.UpdateNewCallbackQuery:
				updateNewCallbackQuery := updateType
				// TODO: если несколько модераторов (нужно проверять флаг в базе по query.MessageId)
				query := updateNewCallbackQuery
				if (query.ChatId != configData.Main) && !hasForwardAnswer(query.ChatId) {
					continue
				}
				// log.Printf("%#v", query)
				fn := func() {
					errorCode := func() int {
						data := ""
						if callbackQueryPayloadData, ok := query.Payload.(*client.CallbackQueryPayloadData); ok {
							data = string(callbackQueryPayloadData.Data)
						}
						if data == "" {
							log.Print("CallbackQueryPayloadData > data is empty")
							return 1001
						}
						a := strings.Split(data, "|")
						if len(a) != 3 {
							log.Print("CallbackQueryPayloadData > invalid data - len() ")
							return 1002
						}
						command := a[0]
						if !contains([]string{"ANSWER", "OK", "CANCEL"}, command) {
							log.Print("CallbackQueryPayloadData > invalid data - command")
							return 1003
						}
						srcId := int64(convertToInt(a[1]))
						if srcId == 0 {
							log.Print("CallbackQueryPayloadData > invalid data - srcId")
							return 1004
						}
						src, err := tdlibClient.GetMessage(&client.GetMessageRequest{
							ChatId:    query.ChatId,
							MessageId: srcId,
						})
						if err != nil {
							log.Print(err)
							return 1005
						}
						payloadData := a[2]
						aa := strings.Split(payloadData, ":")
						if len(aa) != 3 {
							log.Print("CallbackQueryPayloadData > invalid payloadData - len() ")
							return 1006
						}
						sourceChatId := int64(convertToInt(aa[0]))
						if sourceChatId == 0 {
							log.Print("CallbackQueryPayloadData > invalid payloadData - sourceChatId")
							return 1007
						}
						sourceMessageId := int64(convertToInt(aa[1]))
						if sourceMessageId == 0 {
							log.Print("CallbackQueryPayloadData > invalid payloadData - sourceMessageId")
							return 1008
						}
						sourceMediaAlbumId := int64(convertToInt(aa[2]))
						if sourceMediaAlbumId == 0 {
							log.Print("CallbackQueryPayloadData > invalid payloadData - sourceMediaAlbumId")
							return 1009
						} else if sourceMediaAlbumId == -1 {
							sourceMediaAlbumId = 0 // т.к. 0 - значимое значение, то передаётся, как -1
						}
						log.Printf("CallbackQueryPayloadData > command: %s srcId: %d sourceChatId: %d sourceMessageId: %d sourceMediaAlbumId: %d", command, srcId, sourceChatId, sourceMessageId, sourceMediaAlbumId)
						if command == "ANSWER" {
							answer := getAnswer(sourceChatId, sourceMessageId)
							if !addAnswer(sourceChatId, sourceMessageId, answer) {
								log.Print("addAnswer > false")
								return 1016
							}
						} else {
							var messageIds []int64
							// sourceLink := a[2]
							// messageLinkInfo, err := tdlibClient.GetMessageLinkInfo(&client.GetMessageLinkInfoRequest{
							// 	Url: sourceLink,
							// })
							// if err != nil {
							// 	log.Print(err)
							// 	return 1006
							// }
							// if messageLinkInfo.Message == nil {
							// 	log.Print("GetMessageLinkInfo > messageLinkInfo.Message is empty")
							// 	return 1007
							// }
							if sourceMediaAlbumId != 0 {
								messageIds = getMessageIdsByChatMediaAlbumId(sourceChatId, sourceMediaAlbumId)
								if len(messageIds) == 0 {
									log.Print("messageIds is empty")
									return 1011
								}
							} else {
								messageIds = []int64{sourceMessageId}
							}
							for fromChatId, forward := range configData.Forwards {
								if fromChatId == sourceChatId {
									messages, err := tdlibClient.ForwardMessages(&client.ForwardMessagesRequest{
										ChatId: func() int64 {
											if command == "OK" {
												return forward.To
											}
											return configData.Trash
										}(),
										FromChatId: fromChatId,
										MessageIds: messageIds,
									})
									if err != nil {
										log.Print("ForwardMessages > ", err)
										return 1012
									}
									if len(messages.Messages) != int(messages.TotalCount) || messages.TotalCount == 0 {
										log.Print("ForwardMessages > invalid TotalCount")
										return 1013
									}
									// TODO: выставить флаг в базе, что выполнен форвард (если несколько модераторов)
									var messageIds []int64
									if src.MediaAlbumId == 0 {
										messageIds = []int64{src.Id}
									} else {
										messageIds = getMessageIdsByChatMediaAlbumId(src.ChatId, int64(src.MediaAlbumId))
										if len(messageIds) == 0 {
											log.Print("messageIds is empty")
											return 1014
										}
									}
									messageIds = append(messageIds, query.MessageId)
									if _, err := tdlibClient.DeleteMessages(&client.DeleteMessagesRequest{
										ChatId:     query.ChatId,
										MessageIds: messageIds,
									}); err != nil {
										log.Print(err)
										return 1015
									}
									break
								}
							}
						}
						return 0
					}()
					if _, err := tdlibClient.AnswerCallbackQuery(&client.AnswerCallbackQueryRequest{
						CallbackQueryId: query.Id,
						Text: func() string {
							if errorCode > 0 {
								return fmt.Sprintf("Error! %d", errorCode)
							}
							return ""
						}(),
						ShowAlert: true,
					}); err != nil {
						log.Print(err)
						return
					}
				}
				queue.PushBack(fn)
			case *client.UpdateNewMessage:
				updateNewMessage := updateType
				src := updateNewMessage.Message
				if src.IsOutgoing {
					log.Print("src.IsOutgoing > ", src.ChatId)
					continue // !!
				}
				if (src.ChatId != configData.Main) && !hasForward(src.ChatId) {
					continue
				}
				// TODO: системное сообщение отправлять сразу, без задержки в очереди, а уже в очереди его дополнять
				fn := func() {
					log.Printf("UpdateNewMessage > %d:%d", src.ChatId, src.Id)
					if (src.ChatId == configData.Main) || hasForward(src.ChatId) {
						if src.MediaAlbumId != 0 {
							mediaAlbumId := int64(src.MediaAlbumId)
							setMediaAlbumIdByChatMessageId(src.ChatId, src.Id, mediaAlbumId)
							messageIds := getMessageIdsByChatMediaAlbumId(src.ChatId, mediaAlbumId)
							if len(messageIds) > 0 {
								messageIds = append(messageIds, src.Id)
								setMessageIdsByChatMediaAlbumId(src.ChatId, mediaAlbumId, messageIds)
								return
							}
							messageIds = []int64{src.Id}
							setMessageIdsByChatMediaAlbumId(src.ChatId, mediaAlbumId, messageIds)
						}
					}
					if src.ChatId == configData.Main {
						sourceData, ok := getSourceData(src)
						if ok && (src.MediaAlbumId == 0) {
							a := strings.Split(sourceData, ":")
							sourceChatId := int64(convertToInt(a[0]))
							sourceMessageId := int64(convertToInt(a[1]))
							if hasForwardAnswer(sourceChatId) {
								addWaitButton(src.ChatId, src.Id)
								go func() {
									if hasAnswerButton(sourceChatId, sourceMessageId, withRepeat) {
										addAnswerButton(src.ChatId, src.Id, sourceData)
									} else {
										deleteButton(src.ChatId, src.Id)
									}
								}()
							}
						}
						formattedText := func() *client.FormattedText {
							// TODO: https://github.com/tdlib/td/issues/1649
							messageLink, err := tdlibClient.GetMessageLink(&client.GetMessageLinkRequest{
								ChatId:    src.ChatId,
								MessageId: src.Id,
								ForAlbum:  src.MediaAlbumId != 0,
							})
							if err != nil {
								log.Print("GetMessageLink > ", err)
								return nil
							}
							result, err := tdlibClient.ParseTextEntities(&client.ParseTextEntitiesRequest{
								Text: fmt.Sprintf("[⤴️](%s)", messageLink.Link),
								ParseMode: &client.TextParseModeMarkdown{
									Version: 2,
								},
							})
							if err != nil {
								log.Print("ParseTextEntities > ", err)
								return nil
							}
							return result
						}()
						if _, err := tdlibClient.SendMessage(&client.SendMessageRequest{
							ChatId: src.ChatId,
							InputMessageContent: &client.InputMessageText{
								Text: func() *client.FormattedText {
									if formattedText == nil {
										return &client.FormattedText{Text: "#ERROR: Invalid System Message"}
									}
									if sourceData == "" {
										formattedText.Text += " #ERROR: Invalid Source Data"
									}
									return formattedText
								}(),
								DisableWebPagePreview: true,
								ClearDraft:            true,
							},
							Options: &client.MessageSendOptions{
								DisableNotification: true,
							},
							ReplyMarkup: func() client.ReplyMarkup {
								if sourceData == "" || formattedText == nil {
									return nil
								}
								rows := make([][]*client.InlineKeyboardButton, 0)
								btns := make([]*client.InlineKeyboardButton, 0)
								btns = append(btns, &client.InlineKeyboardButton{
									Text: "✅ Yes!",
									Type: &client.InlineKeyboardButtonTypeCallback{
										Data: []byte(fmt.Sprintf("OK|%d|%s", src.Id, sourceData)),
									},
								})
								btns = append(btns, &client.InlineKeyboardButton{
									Text: "🛑 Stop",
									Type: &client.InlineKeyboardButtonTypeCallback{
										Data: []byte(fmt.Sprintf("CANCEL|%d|%s", src.Id, sourceData)),
									},
								})
								rows = append(rows, btns)
								return &client.ReplyMarkupInlineKeyboard{Rows: rows}
							}(),
						}); err != nil {
							log.Print("SendMessage > ", err)
							return
						}
					} else if (src.MediaAlbumId == 0) && hasForwardAnswer(src.ChatId) {
						addWaitButton(src.ChatId, src.Id)
						go func() {
							if hasAnswerButton(src.ChatId, src.Id, withRepeat) {
								sourceData := fmt.Sprintf("%d:%d:-1", src.ChatId, src.Id)
								addAnswerButton(src.ChatId, src.Id, sourceData)
							} else {
								deleteButton(src.ChatId, src.Id)
							}
						}()
					}
				}
				queue.PushBack(fn)
			case *client.UpdateMessageEdited:
				// TODO: не запрашивать hasAnswerButton() снова, а сохранять в БД при UpdateNewMessage и брать там
				// updateMessageEdited := updateType
				// chatId := updateMessageEdited.ChatId
				// messageId := updateMessageEdited.MessageId
				// fn := func() {
				//  log.Printf("UpdateMessageEdited > %d:%d", chatId, messageId)
				// 	isForwardAnswer := hasForwardAnswer(chatId)
				// 	if (chatId == configData.Main) || isForwardAnswer {
				// 		src, err := tdlibClient.GetMessage(&client.GetMessageRequest{
				// 			ChatId:    chatId,
				// 			MessageId: messageId,
				// 		})
				// 		if err != nil {
				// 			log.Print(err)
				// 			return
				// 		}
				// 		if src.MediaAlbumId != 0 {
				// 			return
				// 		}
				// 		if src.ChatId == configData.Main {
				// 			sourceData, ok := getSourceData(src)
				// 			if ok {
				// 				a := strings.Split(sourceData, ":")
				// 				sourceChatId := int64(convertToInt(a[0]))
				// 				sourceMessageId := int64(convertToInt(a[1]))
				// 				if hasForwardAnswer(sourceChatId) {
				// 					if hasAnswerButton(sourceChatId, sourceMessageId, woRepeat) {
				// 						addAnswerButton(src.ChatId, src.Id, sourceData)
				// 					}
				// 				}
				// 			}
				// 		} else if isForwardAnswer {
				// 			if hasAnswerButton(src.ChatId, src.Id, woRepeat) {
				// 				sourceData := fmt.Sprintf("%d:%d:-1", src.ChatId, src.Id)
				// 				addAnswerButton(src.ChatId, src.Id, sourceData)
				// 			}
				// 		}
				// 	}
				// }
				// queue.PushBack(fn)
			case *client.UpdateDeleteMessages:
				updateDeleteMessages := updateType
				if !updateDeleteMessages.IsPermanent {
					continue
				}
				chatId := updateDeleteMessages.ChatId
				if (chatId != configData.Main) && !hasForward(chatId) {
					continue
				}
				messageIds := updateDeleteMessages.MessageIds
				fn := func() {
					log.Printf("UpdateDeleteMessages > %d:%v", chatId, messageIds)
					for _, messageId := range messageIds {
						mediaAlbumId := getMediaAlbumIdByChatMessageId(chatId, messageId)
						if mediaAlbumId == 0 {
							continue
						}
						mediaAlbumMessageIds := getMessageIdsByChatMediaAlbumId(chatId, mediaAlbumId)
						m := make(map[int64]bool)
						for _, mediaAlbumMessageId := range mediaAlbumMessageIds {
							m[mediaAlbumMessageId] = true
						}
						for _, mediaAlbumMessageId := range mediaAlbumMessageIds {
							if messageId == mediaAlbumMessageId {
								deleteMediaAlbumIdByChatMessageId(chatId, messageId)
								m[mediaAlbumMessageId] = false
							}
						}
						a := []int64{}
						for mediaAlbumMessageId, v := range m {
							if v {
								a = append(a, mediaAlbumMessageId)
							}
						}
						if len(a) == 0 {
							deleteMessageIdsByChatMediaAlbumId(chatId, mediaAlbumId)
						} else {
							setMessageIdsByChatMediaAlbumId(chatId, mediaAlbumId, a)
						}
					}
				}
				queue.PushBack(fn)
			}
		}
	}
}

func convertToInt(s string) int {
	i, err := strconv.Atoi(s)
	if err != nil {
		log.Print("convertToInt > ", err)
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
		log.Printf("getByDB > key: %s err: %s", string(key), err)
		// } else {
		// log.Printf("getByDB > key: %s val: %s", string(key), string(val))
	}
	return val
}

func setByDB(key []byte, val []byte) {
	err := badgerDB.Update(func(txn *badger.Txn) error {
		err := txn.Set(key, val)
		return err
	})
	if err != nil {
		log.Printf("setByDB > key: %s err: %s ", string(key), err)
		// } else {
		// log.Printf("setByDB > key: %s val: %s", string(key), string(val))
	}
}

func deleteByDB(key []byte) {
	err := badgerDB.Update(func(txn *badger.Txn) error {
		return txn.Delete(key)
	})
	if err != nil {
		log.Print(err)
	}
}

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

func strLen(s string) int {
	return len(utf16.Encode([]rune(s)))
}

func escapeAll(s string) string {
	// эскейпит все символы: которые нужны для markdown-разметки
	a := []string{
		"_",
		"*",
		`\[`,
		`\]`,
		"(",
		")",
		"~",
		"`",
		">",
		"#",
		"+",
		`\-`,
		"=",
		"|",
		"{",
		"}",
		".",
		"!",
	}
	re := regexp.MustCompile("[" + strings.Join(a, "|") + "]")
	return re.ReplaceAllString(s, `\$0`)
}

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

func setMessageIdsByChatMediaAlbumId(chatId, mediaAlbumId int64, messageIds []int64) {
	key := []byte(fmt.Sprintf("%s:%d:%d", messageIdsByMediaAlbumIdPrefix, chatId, mediaAlbumId))
	log.Printf("setMessageIdsByChatMediaAlbumId > %s %v", string(key), messageIds)
	val, err := json.Marshal(messageIds)
	if err != nil {
		log.Print(err)
		return
	}
	setByDB(key, val)
}

func getMessageIdsByChatMediaAlbumId(chatId, mediaAlbumId int64) []int64 {
	key := []byte(fmt.Sprintf("%s:%d:%d", messageIdsByMediaAlbumIdPrefix, chatId, mediaAlbumId))
	val := getByDB(key)
	if val == nil {
		return nil
	}
	var result []int64
	if err := json.Unmarshal(val, &result); err != nil {
		log.Print(err)
		return nil
	}
	log.Printf("getMessageIdsByChatMediaAlbumId > %s %v", string(key), result)
	return result
}

func deleteMessageIdsByChatMediaAlbumId(chatId, mediaAlbumId int64) {
	key := []byte(fmt.Sprintf("%s:%d:%d", messageIdsByMediaAlbumIdPrefix, chatId, mediaAlbumId))
	log.Printf("deleteMessageIdsByChatMediaAlbumId > %s", string(key))
	deleteByDB(key)
}

const mediaAlbumIdByMessageIdPrefix = "ma-msg"

func setMediaAlbumIdByChatMessageId(chatId, messageId, mediaAlbumId int64) {
	key := []byte(fmt.Sprintf("%s:%d:%d", mediaAlbumIdByMessageIdPrefix, chatId, messageId))
	val := []byte(fmt.Sprintf("%d", mediaAlbumId))
	log.Printf("setMediaAlbumIdByChatMessageId > %s %s", string(key), string(val))
	setByDB(key, val)
}

func getMediaAlbumIdByChatMessageId(chatId, messageId int64) int64 {
	key := []byte(fmt.Sprintf("%s:%d:%d", mediaAlbumIdByMessageIdPrefix, chatId, messageId))
	val := getByDB(key)
	if val == nil {
		return 0
	}
	log.Printf("getMediaAlbumIdByChatMessageId > %s %s", string(key), string(val))
	return int64(convertToInt(string(val)))
}

func deleteMediaAlbumIdByChatMessageId(chatId, messageId int64) {
	key := []byte(fmt.Sprintf("%s:%d:%d", mediaAlbumIdByMessageIdPrefix, chatId, messageId))
	log.Printf("deleteMediaAlbumIdByChatMessageId > %s", string(key))
	deleteByDB(key)
}

const withRepeat = 0
const woRepeat = -1

func hasAnswerButton(chatId, messageId, step int64) bool {
	log.Printf("hasAnswerButton > chatId: %d messageId: %d step: %d", chatId, messageId, step)
	isRepeat := step != woRepeat
	if isRepeat {
		if step >= configData.AnswerRepeat {
			return false
		}
	}
	if configData.AnswerEndpoint == "" {
		err := fmt.Errorf("Config.AnswerEndpoint is empty")
		log.Print(err)
		return false
	}
	if isRepeat {
		time.Sleep(time.Duration(configData.AnswerPause) * time.Second)
	}
	url := fmt.Sprintf("%s/answer?chat_id=%d&message_id=%d&only_check=1&step=%d&rand=%d",
		configData.AnswerEndpoint, chatId, messageId, step, time.Now().UnixNano())
	log.Print(url)
	response, err := http.Get(url)
	if err != nil {
		log.Print(err)
		return false
	}
	defer response.Body.Close()
	log.Printf("%#v", response)
	if isRepeat {
		if response.StatusCode == http.StatusNoContent {
			step++
			return hasAnswerButton(chatId, messageId, step)
		}
	}
	return response.StatusCode == http.StatusOK
	// b, err := httputil.DumpResponse(response, true)
	// if err != nil {
	// 	log.Print(err)
	// }
	// log.Print(string(b))
	// result, err := io.ReadAll(response.Body)
	// if err != nil {
	// 	log.Print(err)
	// 	return
	// }
	// log.Print(string(result))
}

func getSourceData(message *client.Message) (string, bool) { // chatId:messageId:MediaAlbumId
	sourceLink := ""
	if formattedText := getFormattedText(message.Content); formattedText != nil {
		l := len(formattedText.Entities)
		if l > 0 {
			lastEntity := formattedText.Entities[l-1]
			if url, ok := lastEntity.Type.(*client.TextEntityTypeTextUrl); ok {
				sourceLink = url.Url
			}
		}
	}
	if sourceLink == "" {
		log.Print("sourceLink is empty")
		return "", false
	}
	messageLinkInfo, err := tdlibClient.GetMessageLinkInfo(&client.GetMessageLinkInfoRequest{
		Url: sourceLink,
	})
	if err != nil {
		log.Print(err)
	} else if messageLinkInfo.Message == nil {
		log.Print("messageLinkInfo.Message is empty")
	} else {
		return fmt.Sprintf("%d:%d:%d",
			messageLinkInfo.ChatId,
			messageLinkInfo.Message.Id,
			func() int64 {
				mediaAlbumId := int64(messageLinkInfo.Message.MediaAlbumId)
				if mediaAlbumId == 0 {
					return -1
				}
				return mediaAlbumId
			}(),
		), true
	}
	return "", false
}

var queue = list.New()

func runQueue() {
	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()
	for t := range ticker.C {
		_ = t
		// log.Print(t.UTC().Second())
		front := queue.Front()
		if front != nil {
			fn := front.Value.(func())
			fn()
			// This will remove the allocated memory and avoid memory leaks
			queue.Remove(front)
		}
	}
}

func addAnswerButton(chatId, messageId int64, sourceData string) {
	log.Printf("addAnswerButton > chatId: %d messageId: %d sourceData: %s", chatId, messageId, sourceData)
	if _, err := tdlibClient.EditMessageReplyMarkup(&client.EditMessageReplyMarkupRequest{
		ChatId:    chatId,
		MessageId: messageId,
		ReplyMarkup: func() client.ReplyMarkup {
			rows := make([][]*client.InlineKeyboardButton, 0)
			btns := make([]*client.InlineKeyboardButton, 0)
			btns = append(btns, &client.InlineKeyboardButton{
				Text: "Answer", Type: &client.InlineKeyboardButtonTypeCallback{
					Data: []byte(fmt.Sprintf("ANSWER|%d|%s", messageId, sourceData)),
				},
			})
			rows = append(rows, btns)
			return &client.ReplyMarkupInlineKeyboard{Rows: rows}
		}(),
	}); err != nil {
		log.Print(err)
	}
	// TODO: а если кнопка была удалена при редактировании - тоже нужна синхронизация?
}

func hasForward(chatId int64) bool {
	if _, ok := configData.Forwards[chatId]; ok {
		return true
	}
	return false
}

func hasForwardAnswer(chatId int64) bool {
	if forward, ok := configData.Forwards[chatId]; ok && forward.Answer {
		return true
	}
	return false
}

func addWaitButton(chatId, messageId int64) {
	log.Printf("addWaitButton > chatId: %d messageId: %d", chatId, messageId)
	if _, err := tdlibClient.EditMessageReplyMarkup(&client.EditMessageReplyMarkupRequest{
		ChatId:    chatId,
		MessageId: messageId,
		ReplyMarkup: func() client.ReplyMarkup {
			rows := make([][]*client.InlineKeyboardButton, 0)
			btns := make([]*client.InlineKeyboardButton, 0)
			btns = append(btns, &client.InlineKeyboardButton{
				Text: "⏳", Type: &client.InlineKeyboardButtonTypeCallback{
					Data: []byte("..."),
				},
			})
			rows = append(rows, btns)
			return &client.ReplyMarkupInlineKeyboard{Rows: rows}
		}(),
	}); err != nil {
		log.Print(err)
	}
}

func deleteButton(chatId, messageId int64) {
	log.Printf("deleteButton > chatId: %d messageId: %d", chatId, messageId)
	if _, err := tdlibClient.EditMessageReplyMarkup(&client.EditMessageReplyMarkupRequest{
		ChatId:      chatId,
		MessageId:   messageId,
		ReplyMarkup: &client.ReplyMarkupInlineKeyboard{},
		// TODO: не меняет статус сообщения на "изменено"
		// func() client.ReplyMarkup {
		// 	rows := make([][]*client.InlineKeyboardButton, 0)
		// 	btns := make([]*client.InlineKeyboardButton, 0)
		// 	btns =append(btns, &client.InlineKeyboardButton{
		// 		Text: "❌", Type: &client.InlineKeyboardButtonTypeCallback{
		// 			Data: []byte("..."),
		// 		},
		// 	})
		// 	rows = append(rows, btns)
		// 	return &client.ReplyMarkupInlineKeyboard{Rows: rows}
		// }(),
	}); err != nil {
		log.Print(err)
	}
}

func getAnswer(chatId, messageId int64) string {
	log.Printf("getAnswer > chatId: %d messageId: %d", chatId, messageId)
	if configData.AnswerEndpoint == "" {
		err := fmt.Errorf("Config.AnswerEndpoint is empty")
		log.Print(err)
		return ""
	}
	url := fmt.Sprintf("%s/answer?chat_id=%d&message_id=%d&only_check=0&rand=%d",
		configData.AnswerEndpoint, chatId, messageId, time.Now().UnixNano())
	log.Print(url)
	response, err := http.Get(url)
	if err != nil {
		log.Print(err)
		return ""
	}
	defer response.Body.Close()
	log.Printf("%#v", response)
	if response.StatusCode != http.StatusOK {
		return ""
	}
	// b, err := httputil.DumpResponse(response, true)
	// if err != nil {
	// 	log.Print(err)
	// }
	// log.Print(string(b))
	result, err := io.ReadAll(response.Body)
	if err != nil {
		log.Print(err)
		return ""
	}
	return string(result)
}

func addAnswer(chatId, messageId int64, answer string) bool {
	if answer == "" {
		log.Print("addAnswer > answer is empty")
		return false
	}
	answerFormattedText, err := tdlibClient.ParseTextEntities(&client.ParseTextEntitiesRequest{
		Text: escapeAll(answer),
		ParseMode: &client.TextParseModeMarkdown{
			Version: 2,
		},
	})
	if err != nil {
		log.Print("ParseTextEntities > ", err)
		return false
	}
	message, err := tdlibClient.GetMessage(&client.GetMessageRequest{
		ChatId:    chatId,
		MessageId: messageId,
	})
	if err != nil {
		log.Print("GetMessage > ", err)
		return false
	}
	formattedText := getFormattedText(message.Content)
	if formattedText == nil {
		log.Print("getFormattedText > result is empty")
		return false
	}
	offset := int32(strLen(formattedText.Text))
	if offset > 0 {
		formattedText.Text += "\n\n"
		offset = offset + 2
	}
	for _, entity := range answerFormattedText.Entities {
		entity.Offset += offset
	}
	formattedText.Text += answerFormattedText.Text
	formattedText.Entities = append(formattedText.Entities, answerFormattedText.Entities...)
	if _, err := tdlibClient.EditMessageText(&client.EditMessageTextRequest{
		ChatId:    chatId,
		MessageId: messageId,
		InputMessageContent: &client.InputMessageText{
			Text:                  formattedText,
			DisableWebPagePreview: true,
			ClearDraft:            true,
		},
		ReplyMarkup: message.ReplyMarkup,
	}); err != nil {
		log.Print("EditMessageText > ", err)
		return false
	}
	return true
}
