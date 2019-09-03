package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"sync"
	"time"

	routing "github.com/qiangxue/fasthttp-routing"
	"github.com/valyala/fasthttp"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
	"gopkg.in/olebedev/go-duktape.v3"
)

func main() {
	//	connectOnMongo()
	httpServer()
	wait()
}

func run(query string, data interface{}) (interface{}, error) {
	ctx := duktape.New()

	c := make(chan interface{}, 1)

	ctx.PushGlobalGoFunction("exec", func(ctx *duktape.Context) int {
		c <- runCommand(ctx.SafeToString(-1))
		return 0
	})

	path := os.Getenv("QUERY_PATH")
	if path == "" {
		path = "./query"
	}

	filepath := path + "/" + query + ".js"

	log.Println(filepath)

	if _, err := os.Stat(filepath); os.IsNotExist(err) {
		return nil, fmt.Errorf("file not found")
	}

	err := ctx.PevalFile(filepath)

	if err != nil {
		return nil, err
	}

	dataStr := "{}"

	if data != nil {
		dataBt, _ := json.Marshal(data)
		dataStr = string(dataBt)
	}

	err = ctx.PevalString(`exec(JSON.stringify(command(` + dataStr + `)))`)

	if err != nil {
		return nil, err
	}

	ctx.Pop()
	ctx.DestroyHeap()

	return <-c, nil
}

func httpServer() {
	router := routing.New()

	router.Use(corsHandler(), logHandler(), panicHandler())

	router.Any("/<query>", func(ctx *routing.Context) error {
		payload := string(ctx.PostBody())

		if payload == "" {
			payload = "{}"
		}

		var body interface{}
		err := json.Unmarshal([]byte(payload), &body)

		if checkError(ctx, err) {
			return nil
		}

		queryFile := ctx.Param("query")

		result, err := run(queryFile, body)

		if checkError(ctx, err) {
			return nil
		}

		responseBody, err := json.Marshal(result)

		if checkError(ctx, err) {
			return nil
		}

		ctx.SetBody(responseBody)
		ctx.SetStatusCode(fasthttp.StatusOK)

		return nil
	})

	fasthttp.ListenAndServe("0.0.0.0:8080", router.HandleRequest)
}

func wait() {
	wg := &sync.WaitGroup{}
	wg.Add(1)
	wg.Wait()
}

var mongoTries = 0
var mongoClient *mongo.Client

func connectOnMongo() *mongo.Client {
	log.Println("Connecting on MongoDB...")
	opt := &options.ClientOptions{}
	client, err := mongo.NewClient(opt.ApplyURI(os.Getenv("MONGO_DSN")))
	check(err)

	err = client.Connect(context.TODO())

	if err != nil {
		time.Sleep(time.Second * 1)
		log.Println(err.Error())
		mongoTries++
		if mongoTries > 10 {
			log.Panic(err)
		}
		log.Println("Trying again...")
		return connectOnMongo()
	} else {
		return client
	}
}

func runCommand(commandStr string) interface{} {
	db := mongoClient.Database(os.Getenv("MONGO_DB"))

	ctx, _ := context.WithTimeout(context.Background(), 5*time.Minute)

	command := bson.M{}
	json.Unmarshal([]byte(commandStr), &command)

	result := db.RunCommand(ctx, command, &options.RunCmdOptions{})
	check(result.Err())

	ret := map[string]interface{}{}
	result.Decode(&ret)

	return ret
}

func check(err error) {
	if err != nil {
		panic(err)
	}
}

func corsHandler() routing.Handler {
	return func(c *routing.Context) (err error) {
		c.Response.Header.Set("Access-Control-Allow-Origin", "*")
		c.Response.Header.Set("Access-Control-Allow-Headers", "Origin, X-Requested-With, Content-Type, Accept, Authorization")
		c.Response.Header.Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS, HEAD")
		c.Response.Header.Set("Access-Control-Allow-Credentials", "true")
		if bytes.Equal(c.Method(), []byte("OPTIONS")) {
			c.SetStatusCode(fasthttp.StatusOK)
			return nil
		}
		r := c.Next()
		return r
	}
}

func logHandler() routing.Handler {
	return func(c *routing.Context) (err error) {
		log.Println("Request: ", string(c.Method())+" "+string(c.Request.RequestURI()))
		log.Println("Request Body: ", string(c.PostBody()))
		r := c.Next()
		log.Println("Response Status: ", c.Response.StatusCode())
		//log.Println("Response Body: ", string(c.Response.Body()))
		return r
	}
}

func panicHandler() routing.Handler {
	return func(c *routing.Context) (err error) {
		defer func() {
			if e := recover(); e != nil {
				c.Response.Header.SetStatusCode(fasthttp.StatusInternalServerError)
				eStr := fmt.Sprintf("%v", e)
				c.SetBody([]byte(eStr))
			}
		}()

		r := c.Next()
		return r
	}
}

func checkError(ctx *routing.Context, err error) bool {
	if err != nil {
		responseBody, _ := json.Marshal(map[string]string{"error": fmt.Sprintf("%v", err.Error())})
		ctx.SetBody(responseBody)
		ctx.SetStatusCode(fasthttp.StatusInternalServerError)
		return true
	}

	return false
}
