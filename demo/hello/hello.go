package main

import (
	"log"
	"net/http"
	"time"
)

func main() {
	go func() {
		for {
			log.Println("Hello World")

			time.Sleep(time.Second)
		}
	}()

	http.HandleFunc("/", func(writer http.ResponseWriter, request *http.Request) {
		writer.Write([]byte("Hello World from LocalFlux demo!"))
	})

	err := http.ListenAndServe(":8080", nil)
	if err != nil {
		log.Fatal("ListenAndServe: ", err)
	}
}
