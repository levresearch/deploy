package main

import (
	"flag"
	"fmt"
	"os"
)

func main() {
	flag.Usage = func() {
		fmt.Fprint(os.Stdout, `deploy - dynamically deploy managed services on servers you own

usage:
  deploy [flags]

flags:
  -h, --help   show this help
`)
	}

	flag.Parse()
}
