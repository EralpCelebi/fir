package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"time"

	"github.com/fatih/color"
	"github.com/openai/openai-go"
)

type Shell struct {
	cmd        *exec.Cmd
	stdinPipe  io.WriteCloser
	stdoutPipe io.ReadCloser
	stderrPipe io.ReadCloser
	outScanner *bufio.Scanner
	errScanner *bufio.Scanner
	results    bytes.Buffer
	lastRead   time.Time
	doneRead   bool
}

func CreateShell() *Shell {
	cmd := exec.Command("sh")

	shell := new(Shell)

	shell.cmd = cmd
	shell.stdinPipe, _ = cmd.StdinPipe()
	shell.stdoutPipe, _ = cmd.StdoutPipe()
	shell.stderrPipe, _ = cmd.StderrPipe()

	shell.outScanner = bufio.NewScanner(shell.stdoutPipe)
	shell.errScanner = bufio.NewScanner(shell.stderrPipe)

	if err := cmd.Start(); err != nil {
		log.Fatal("Couldn't start command receiver.", err)
	}

	go func() {
		for {
			if shell.outScanner.Scan() {
				text := shell.outScanner.Bytes()
				shell.lastRead = time.Now()
				shell.results.Write(text)
			}
		}
	}()

	go func() {
		for {
			if shell.errScanner.Scan() {
				text := shell.errScanner.Bytes()
				shell.lastRead = time.Now()
				shell.results.Write(text)
			}
		}
	}()

	go func() {
		timeout := time.NewTimer(1 * time.Millisecond)
		for {
			select {
			case <-timeout.C:
				timeout.Reset(1 * time.Millisecond)
				shell.doneRead = time.Since(shell.lastRead) > 100*time.Millisecond
			default:
				continue
			}
		}
	}()

	return shell
}

func (shell *Shell) Execute(command string) string {
	shell.stdinPipe.Write([]byte("pwd\n"))
	shell.doneRead = false

	for !shell.doneRead {
		time.Sleep(200 * time.Microsecond)
	}

	pwd := shell.results.String()
	shell.results.Reset()

	os.Chdir(pwd)

	shell.stdinPipe.Write([]byte(command + "\n"))
	shell.doneRead = false

	for !shell.doneRead {
		time.Sleep(200 * time.Microsecond)
	}

	return shell.results.String()
}

func main() {
	ctx := context.Background()

	client := openai.NewClient()

	params := openai.ChatCompletionNewParams{
		Messages: []openai.ChatCompletionMessageParamUnion{
			openai.DeveloperMessage("Greet the user."),
		},
		Tools: []openai.ChatCompletionToolParam{
			{
				Function: openai.FunctionDefinitionParam{
					Name:        "execute",
					Description: openai.String("Executes command in users local shell."),
					Parameters: openai.FunctionParameters{
						"type": "object",
						"properties": map[string]any{
							"command": map[string]string{
								"type": "string",
							},
						},
						"required": []string{"command"},
					},
				},
			},
			{
				Function: openai.FunctionDefinitionParam{
					Name:        "read",
					Description: openai.String("Reads a file locally."),
					Parameters: openai.FunctionParameters{
						"type": "object",
						"properties": map[string]any{
							"path_to_file": map[string]string{
								"type": "string",
							},
						},
						"required": []string{"path_to_file"},
					},
				},
			},
			{
				Function: openai.FunctionDefinitionParam{
					Name:        "write",
					Description: openai.String("Write to a file locally. Creates files if necessary."),
					Parameters: openai.FunctionParameters{
						"type": "object",
						"properties": map[string]any{
							"path_to_file": map[string]string{
								"type": "string",
							},
							"content_to_write": map[string]string{
								"type": "string",
							},
						},
						"required": []string{"path_to_file", "content_to_write"},
					},
				},
			},
		},
		Seed:  openai.Int(0),
		Model: openai.ChatModelGPT4oMini,
	}

	completion, err := client.Chat.Completions.New(ctx, params)

	if err != nil {
		log.Fatal(err)
	}

	color.RGB(10, 10, 200).Println(completion.Choices[0].Message.Content)
	params.Messages = append(params.Messages, completion.Choices[0].Message.ToParam())

	shell := CreateShell()
	reader := bufio.NewReader(os.Stdin)

	for {
		color.RGB(200, 10, 10).Print("Question")
		color.RGB(100, 200, 10).Print("% ")

		inputBytes, _, _ := reader.ReadLine()
		input := string(inputBytes)

		params.Messages = append(params.Messages, openai.UserMessage(input))

		completion, err := client.Chat.Completions.New(ctx, params)
		if err != nil {
			log.Fatal(err)
		}

		color.RGB(10, 10, 200).Println(completion.Choices[0].Message.Content)

		params.Messages = append(params.Messages, completion.Choices[0].Message.ToParam())

		if len(completion.Choices[0].Message.ToolCalls) == 0 {
			continue
		}

	HandleCalls:
		for _, tool_call := range completion.Choices[0].Message.ToolCalls {
			name := tool_call.Function.Name

			log.Println("Call made to function", name, tool_call.ID)

			var (
				call_args map[string]any
			)

			if err := json.Unmarshal([]byte(tool_call.Function.Arguments), &call_args); err != nil {
				log.Fatal("Couldn't parse requested function arguments.", err)
			}

			switch name {
			case "execute":
				command := call_args["command"].(string)

				log.Println("Executing", command)
				message := shell.Execute(command)

				params.Messages = append(params.Messages, openai.ToolMessage(message, tool_call.ID))
			case "read":
				path_to_file := call_args["path_to_file"].(string)

				log.Println("Reading from", path_to_file)

				shell.Execute("pwd")
				file_contents, err := os.ReadFile(path_to_file)

				if err != nil {
					log.Println("Couldn't read from file.", err)
					file_contents = []byte("Couldn't read from file.")
				}

				params.Messages = append(params.Messages, openai.ToolMessage(string(file_contents), tool_call.ID))
			case "write":
				path_to_file := call_args["path_to_file"].(string)
				content_to_write := call_args["content_to_write"].(string)

				log.Println("Writing to", path_to_file)

				shell.Execute("pwd")
				err := os.WriteFile(path_to_file, []byte(content_to_write), 0644)

				if err != nil {
					log.Println("Couldn't write to file.", err)
					params.Messages = append(params.Messages, openai.ToolMessage("Couldn't write to file.", tool_call.ID))
				} else {
					params.Messages = append(params.Messages, openai.ToolMessage("Written to file successfuly.", tool_call.ID))
				}

			default:
				log.Println("We shouldn't be here.")
			}
		}

		completion, err = client.Chat.Completions.New(ctx, params)
		if err != nil {
			log.Fatal("Couldn't get results.", err)
		}

		params.Messages = append(params.Messages, completion.Choices[0].Message.ToParam())

		fmt.Println()
		color.RGB(10, 10, 200).Println(completion.Choices[0].Message.Content)

		if len(completion.Choices[0].Message.ToolCalls) != 0 {
			goto HandleCalls
		}
	}
}
