package config

const filename = ".config.yml"

type Config struct {
	Main           int64
	Trash          int64
	Forwards       map[int64]Forward // map[From]Forward
	AnswerEndpoint string
	AnswerPause    int64 // in milliseconds, TODO: подкручивать паузу по накопленной статистике про step в budva32
	AnswerRepeat   int64
}

type Forward struct {
	Answer bool
	To     int64
}
