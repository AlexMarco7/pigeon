package pigeon

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"sync"
	"text/template"
	"time"

	"go.mongodb.org/mongo-driver/mongo/readpref"

	"github.com/joho/godotenv"
	routing "github.com/qiangxue/fasthttp-routing"
	"github.com/spf13/cast"
	"gopkg.in/olebedev/go-duktape.v3"

	//"github.com/robertkrimen/otto"
	"github.com/valyala/fasthttp"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

func Start() {
	godotenv.Load()
	mongoClient = connectOnMongo()
	httpServer()
	wait()
}

func httpServer() {
	router := routing.New()

	router.Use(corsHandler(), logHandler(), panicHandler())

	router.Any("/<query>", func(ctx *routing.Context) error {
		body := map[string]interface{}{}

		if ctx.IsPost() || ctx.IsPut() {
			payload := string(ctx.PostBody())
			err := json.Unmarshal([]byte(payload), &body)

			if checkError(ctx, err) {
				return nil
			}
		} else {
			ctx.QueryArgs().VisitAll(func(key, value []byte) {
				body[string(key)] = string(value)
			})
		}

		queryFile := ctx.Param("query")

		result, err := run(queryFile, body)

		if checkError(ctx, err) {
			return nil
		}

		ctx.Success("application/json", []byte(result.(string)))
		return nil
	})

	router.Any("/<query>/<view>", func(ctx *routing.Context) error {
		body := map[string]interface{}{}

		if ctx.IsPost() || ctx.IsPut() {
			payload := string(ctx.PostBody())
			err := json.Unmarshal([]byte(payload), &body)

			if checkError(ctx, err) {
				return nil
			}
		} else {
			ctx.QueryArgs().VisitAll(func(key, value []byte) {
				body[string(key)] = string(value)
			})
		}

		queryFile := ctx.Param("query")

		result, err := run(queryFile, body)

		if checkError(ctx, err) {
			return nil
		}

		view := ctx.Param("view")

		html := render(view, result.(string))

		ctx.Success("text/html", []byte(html))
		return nil
	})

	fasthttp.ListenAndServe("0.0.0.0:8080", router.HandleRequest)
}

func render(view string, data string) string {
	path := os.Getenv("VIEW_PATH")
	if path == "" {
		path = "./view"
	}

	filepath := path + "/" + view + ".html"

	var tpl bytes.Buffer

	tmpl, err := template.ParseFiles(filepath)

	if err != nil {
		return err.Error()
	}

	tmpl.Execute(&tpl, data)

	return tpl.String()
}

func run(query string, data interface{}) (interface{}, error) {
	ctx := duktape.New()

	ctx.PushGlobalGoFunction("exec", func(ctx *duktape.Context) int {
		params := ctx.SafeToString(-1)
		result := runCommand(params)
		js, _ := json.Marshal(result)
		str := string(js)
		ctx.PushString(str)
		return 1
	})

	ctx.PushGlobalGoFunction("log", func(c *duktape.Context) int {
		fmt.Println(c.SafeToString(-1))
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

	err = ctx.PevalString(`
	   JSON.stringify(transform(JSON.parse(exec(JSON.stringify(command(` + dataStr + `)))),` + dataStr + `))
	`)

	result := ctx.GetString(-1)

	if err != nil {
		return nil, err
	}

	ctx.Pop()
	ctx.DestroyHeap()

	return result, nil
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
	opt := &options.ClientOptions{
		ReadPreference: readpref.SecondaryPreferred(),
	}
	client, err := mongo.NewClient(opt.ApplyURI(os.Getenv("MONGODB_DSN")))
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
		log.Println("Connected!")
		return client
	}
}

func runCommand(commandStr string) interface{} {

	if commandStr == "null" || commandStr == "" {
		return nil
	}

	db := mongoClient.Database(os.Getenv("MONGODB_DATABASE"))

	ctx, _ := context.WithTimeout(context.Background(), 20*time.Minute)

	command := bson.M{}
	json.Unmarshal([]byte(commandStr), &command)

	cmdMap := map[string]interface{}(command)
	cmdMap = checkDates(&cmdMap).(map[string]interface{})

	orderedCommand := bson.D{}
	for k, v := range cmdMap {
		if k == "aggregate" {
			orderedCommand = append(orderedCommand, bson.E{k, v})
		}
	}
	for k, v := range cmdMap {
		if k != "aggregate" {
			orderedCommand = append(orderedCommand, bson.E{k, v})
		}
	}

	log.Printf("%#v", orderedCommand)

	result := db.RunCommand(ctx, orderedCommand, &options.RunCmdOptions{})
	check(result.Err())

	var ret bson.M
	result.Decode(&ret)

	return ret
}

func checkDates(src interface{}) interface{} {
	switch (src).(type) {
	case *map[string]interface{}:
		p := (src).(*map[string]interface{})
		for k, v := range *p {
			switch v.(type) {
			case []interface{}:
				{
					for idx, i := range v.([]interface{}) {
						switch i.(type) {
						case map[string]interface{}:
							{
								m := i.(map[string]interface{})
								v.([]interface{})[idx] = checkDates(&m)
							}
						}
					}
				}
			case map[string]interface{}:
				{
					m := v.(map[string]interface{})
					(*p)[k] = checkDates(&m)
				}
			case string:
				{
					if k == "$toDate" {
						dt := cast.ToTime(v)
						return dt
					}
				}
			}
		}
		return *p
	}
	return nil
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
		log.Println("Response Body: ", string(c.Response.Body()))
		return r
	}
}

func panicHandler() routing.Handler {
	return func(c *routing.Context) (err error) {
		defer func() {
			if e := recover(); e != nil {
				eStr := fmt.Sprintf("%v", e)
				responseBody, _ := json.Marshal(map[string]string{"error": eStr})
				c.SetBody(responseBody)
				c.SetStatusCode(fasthttp.StatusInternalServerError)
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
