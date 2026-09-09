package main

import (
	"fmt"
	"os"

	"miniai/internal/powerleasehelper"
)

func main() {
	if os.Geteuid() != 0 {
		fmt.Fprintln(os.Stderr, "miniai-power-helper must run as root")
		os.Exit(1)
	}
	result, err := powerleasehelper.New().Execute(os.Args[1:])
	if err != nil {
		fmt.Fprintln(os.Stderr, "miniai-power-helper operation failed")
		os.Exit(1)
	}
	fmt.Println(result)
}
