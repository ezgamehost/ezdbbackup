package main

import (
	"crypto/rand"
	"fmt"
	"io"
	"os"
	"strconv"
)

func main() {
	for _, arg := range os.Args[1:] {
		if arg == "--version" {
			fmt.Println("mysqldump fake 1.0")
			return
		}
	}
	if os.Getenv("FAKE_DUMP_FAIL") == "1" {
		fmt.Fprintf(os.Stderr, "forced dump failure: %s\n", os.Getenv("MYSQL_PWD"))
		os.Exit(9)
	}
	if configured := os.Getenv("FAKE_DUMP_RANDOM_BYTES"); configured != "" {
		count, err := strconv.ParseInt(configured, 10, 64)
		if err != nil || count < 1 || count > 64<<20 {
			fmt.Fprintln(os.Stderr, "FAKE_DUMP_RANDOM_BYTES must be between 1 and 67108864")
			os.Exit(2)
		}
		if _, err := io.CopyN(os.Stdout, rand.Reader, count); err != nil {
			fmt.Fprintln(os.Stderr, "generate random fake dump")
			os.Exit(3)
		}
		return
	}
	fmt.Println("CREATE TABLE example(id INT);")
	fmt.Println("INSERT INTO example VALUES (1);")
}
