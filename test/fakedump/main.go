package main

import (
	"fmt"
	"os"
)

func main() {
	for _, arg := range os.Args[1:] {
		if arg == "--version" {
			fmt.Println("mysqldump fake 1.0")
			return
		}
	}
	if os.Getenv("FAKE_DUMP_FAIL") == "1" {
		fmt.Fprintln(os.Stderr, "forced dump failure")
		os.Exit(9)
	}
	fmt.Println("CREATE TABLE example(id INT);")
	fmt.Println("INSERT INTO example VALUES (1);")
}
