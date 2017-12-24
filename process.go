package procs

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"sync"
)

// Process is intended to be used like exec.Cmd where possible.
type Process struct {
	// CmdString takes a string and parses it into the relevant cmds
	CmdString string

	// Cmds is the list of command delmited by pipes.
	Cmds []*exec.Cmd

	// Env provides a map[string]string that can mutated before
	// running a command.
	Env map[string]string

	// Dir defines the directory the command should run in. The
	// Default is the current dir.
	Dir string

	// OutputHandler can be defined to perform any sort of processing
	// on the output. The simple interface is to accept a string (a
	// line of output) and return a string that will be included in the
	// buffered output and/or output written to stdout.
	OutputHandler func(string) string

	// When no output is given, we'll buffer output in these vars.
	errBuffer bytes.Buffer
	outBuffer bytes.Buffer

	// When a output handler is provided, we ensure we're handling a
	// single line at at time.
	outputWait *sync.WaitGroup
}

// NewProcess creates a new *Process from a command string.
func NewProcess(command string) *Process {
	return &Process{CmdString: command}
}

// internal expand method to use the os env or proc env.
func (p *Process) expand(s string) string {
	if p.Env != nil {
		return os.ExpandEnv(s)
	}

	return os.Expand(s, func(key string) string {
		v, _ := p.Env[key]
		return v
	})
}

// addCmd adds a new command to the list of commands, ensuring the Dir
// and Env have been added to the underlying *exec.Cmd instances.
func (p *Process) addCmd(cmdparts []string) {
	var cmd *exec.Cmd
	if len(cmdparts) == 1 {
		cmd = exec.Command(cmdparts[0])
	} else {
		cmd = exec.Command(cmdparts[0], cmdparts[1:]...)
	}

	if p.Dir != "" {
		cmd.Dir = p.Dir
	}

	if p.Env != nil {
		env := []string{}
		for k, v := range p.Env {
			env = append(env, fmt.Sprintf("%s=%s", k, p.expand(v)))
		}

		cmd.Env = env
	}

	p.Cmds = append(p.Cmds, cmd)
}

// findCmds parses the CmdString to find the commands that should be
// run by spliting the lexically parsed command by pipes ("|").
func (p *Process) findCmds() {
	// Skip if the cmd set is already set. This allows manual creation
	// of piped commands.
	if len(p.Cmds) > 0 {
		return
	}

	if p.CmdString == "" {
		return
	}

	parts := SplitCommand(p.CmdString)
	for i := range parts {
		parts[i] = p.expand(parts[i])
	}

	cmd := []string{}
	for _, part := range parts {
		if part == "|" {
			p.addCmd(cmd)
			cmd = []string{}
		} else {
			cmd = append(cmd, part)
		}
	}

	p.addCmd(cmd)
}

// lineReader takes will read a line in the io.Reader and write to the
// Process output buffer and use any OutputHandler that exists.
func (p *Process) lineReader(wg *sync.WaitGroup, r io.Reader) {
	defer wg.Done()

	reader := bufio.NewReader(r)
	var buffer bytes.Buffer

	for {
		buf := make([]byte, 1024)

		if n, err := reader.Read(buf); err != nil {
			return
		} else {
			buf = buf[:n]
		}

		for {
			i := bytes.IndexByte(buf, '\n')
			if i < 0 {
				break
			}

			buffer.Write(buf[0:i])
			outLine := buffer.String()
			if p.OutputHandler != nil {
				outLine = p.OutputHandler(outLine)
			}
			p.outBuffer.WriteString(outLine)
			buffer.Reset()
			buf = buf[i+1:]
		}
		buffer.Write(buf)
	}
}

// checkErr shortens the creation of the pipes by bailing out with a
// log.Fatal.
func checkErr(msg string, err error) {
	if err != nil {
		log.Fatalf("%s: %s", msg, err)
	}
}

// setupOutputHandler configures the last cmd in the list of cmds to
// use the output handler defined.
func (p *Process) setupOutputHandler(cmd *exec.Cmd) error {
	stdout, err := cmd.StdoutPipe()
	checkErr("error creating stdout pipe", err)

	stderr, err := cmd.StderrPipe()
	checkErr("error creating stderr pipe", err)

	p.outputWait = new(sync.WaitGroup)
	p.outputWait.Add(2)

	// These close the stdout/err channels
	go p.lineReader(p.outputWait, stdout)
	go p.lineReader(p.outputWait, stderr)

	return nil
}

func (p *Process) setupPipes() error {
	last := len(p.Cmds) - 1

	for i, cmd := range p.Cmds[:last] {
		var err error

		p.Cmds[i+1].Stdin, err = cmd.StdoutPipe()
		if err != nil {
			fmt.Printf("error creating stdout pipe: %s\n", err)
			return err
		}

		cmd.Stderr = &p.errBuffer
	}

	err := p.setupOutputHandler(p.Cmds[last])
	return err
}

func (p *Process) start() error {
	for i, cmd := range p.Cmds {
		err := cmd.Start()
		if err != nil {
			// Wait for command that ran earlier to finish.
			defer func() {
				for _, precmd := range p.Cmds[0:i] {
					precmd.Wait()
				}
			}()
			return err
		}
	}

	return nil
}

func (p *Process) wait() error {
	for _, cmd := range p.Cmds {
		cmd.Wait()
	}
	return nil
}

// Run executes the cmds and returns the output as a string and any error.
func (p *Process) Run() (string, error) {
	p.findCmds()
	p.setupPipes()

	if err := p.start(); err != nil {
		fmt.Printf("error calling command: %q\n", err)
		fmt.Println(string(p.errBuffer.Bytes()))
		return "", err
	}

	p.wait()

	return p.outBuffer.String(), nil
}

// Start will start the list of cmds.
func (p *Process) Start() error {
	p.findCmds()
	p.setupPipes()

	if err := p.start(); err != nil {
		fmt.Printf("error calling command: %q\n", err)
		fmt.Println(string(p.errBuffer.Bytes()))
		return err
	}

	for i, cmd := range p.Cmds {
		err := cmd.Start()
		if err != nil {
			defer func() {
				for _, precmd := range p.Cmds[0:i] {
					precmd.Wait()
				}
			}()
			return err
		}
	}

	return nil
}

// Wait will block, waiting for the commands to finish.
func (p *Process) Wait() error {
	if p.outputWait != nil {
		p.outputWait.Wait()
	}

	last := len(p.Cmds) - 1
	return p.Cmds[last].Wait()
}

// Output returns the buffered output as a string.
func (p *Process) Output() string {
	return p.outBuffer.String()
}
