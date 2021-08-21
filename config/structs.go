package config

const filename = ".config.yml"

type Config struct {
	Main           int64
	Trash          int64
	Forwards       map[int64]Forward // map[From]Forward
	AnswerEndpoint string
}

type Forward struct {
	Answer bool
	To     int64
}
