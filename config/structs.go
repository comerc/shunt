package config

const filename = ".config.yml"

type Config struct {
	Main     int64
	Cancel   int64
	Forwards []Forward
}

type Forward struct {
	From   int64
	CopyTo int64
}
