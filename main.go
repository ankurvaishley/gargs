package main

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"log"
	"os"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/alexflint/go-arg"
	"github.com/brentp/gargs/process"
	"github.com/fatih/color"
	isatty "github.com/mattn/go-isatty"
	"github.com/valyala/fasttemplate"
)

// Version is the current version
const Version = "0.3.6"

// ExitCode is the highest exit code seen in any command
var ExitCode = 0

// Params are the user-specified command-line arguments
type Params struct {
	Procs       int      `arg:"-p,help:number of processes to use."`
	Nlines      int      `arg:"-n,help:number of lines to consume for each command. -s and -n are mutually exclusive."`
	Retry       int      `arg:"-r,help:number of times to retry a command if it fails (default is 0)."`
	Ordered     bool     `arg:"-o,help:keep output in order of input."`
	Sep         string   `arg:"-s,help:regular expression split line with to fill multiple template spots default is not to split. -s and -n are mutually exclusive."`
	Verbose     bool     `arg:"-v,help:print commands to stderr as they are executed."`
	StopOnError bool     `arg:"-s,--stop-on-error,help:stop execution on any error. default is to report errors and continue execution."`
	DryRun      bool     `arg:"-d,--dry-run,help:print (but do not run) the commands."`
	Log         string   `arg:"-l,--log,help:file to log commands. Successful commands are prefixed with '#'."`
	Command     string   `arg:"positional,required,help:command to execute."`
	log         *os.File `arg:"-"`
}

// Version string for go-args
func (p Params) Version() string {
	return "gargs " + Version
}

// isStdin checks if we are getting data from stdin.
func isStdin() bool {
	// http://stackoverflow.com/a/26567513
	stat, err := os.Stdin.Stat()
	if err != nil {
		return false
	}
	return (stat.Mode() & os.ModeCharDevice) == 0
}

func main() {
	args := Params{Procs: 1, Nlines: 1}
	p := arg.MustParse(&args)
	if args.Sep != "" && args.Nlines > 1 {
		p.Fail("must specify either sep (-s) or n-lines (-n), not both")
	}
	// if neither is specified then we default to whitespace
	if args.Nlines == 1 && args.Sep == "" {
		args.Sep = "\\s+"
	}
	if !isStdin() {
		fmt.Fprintln(os.Stderr, color.RedString("ERROR: expecting input on STDIN"))
		os.Exit(255)
	}
	if args.Log != "" {
		var err error
		args.log, err = os.Create(args.Log)
		check(err)
	}
	runtime.GOMAXPROCS(args.Procs)
	run(args)
	os.Exit(ExitCode)
}

func check(e error) {
	if e != nil {
		log.Fatal(e)
	}
}

func handleCommand(args *Params, cmd string, ch chan string) {
	if args.DryRun {
		fmt.Fprintf(os.Stdout, "%s\n", cmd)
		return
	}
	ch <- cmd
}

func fillTmplMap(toks []string, line string) map[string]interface{} {
	m := make(map[string]interface{}, 5)
	if toks != nil {
		for i, t := range toks {
			m[strconv.FormatInt(int64(i), 10)] = t
		}
	}
	m["Line"] = line
	return m
}

func getScanner() *bufio.Scanner {
	scanner := bufio.NewScanner(os.Stdin)
	scanner.Buffer(make([]byte, 0, 16384), 5e9)
	//if rs := os.Getenv("RS"); rs != "" && rs != "\n" && rs != "\r\n" {
	if rs := os.Getenv("RS"); rs != "" {
		brs := []byte(rs)
		scanner.Split(func(data []byte, atEOF bool) (advance int, token []byte, err error) {
			if atEOF && len(data) == 0 {
				return 0, nil, nil
			}
			if i := bytes.Index(data, brs); i >= 0 {
				// Note that by adding the length here, we include the delimiter.
				return i + 1, data[:i+len(brs)], nil
			}
			if atEOF {
				return len(data), data, nil
			}
			return 0, nil, nil
		})
	}
	return scanner
}

func genCommands(args *Params, tmpl *fasttemplate.Template) <-chan string {
	ch := make(chan string)
	var resep *regexp.Regexp
	if args.Sep != "" {
		resep = regexp.MustCompile(args.Sep)
	}

	scanner := getScanner()
	fs := os.Getenv("FS")
	if fs == "" {
		fs = " "
	}

	go func() {
		var lines []string
		if resep == nil {
			lines = make([]string, 0, args.Nlines)
		}
		var buf bytes.Buffer
		for scanner.Scan() {
			buf.Reset()
			line := scanner.Text()
			serr := scanner.Err()
			if serr == nil || (serr == io.EOF && len(line) > 0) {
				// TODO: make dropping bytes optional.
				if resep != nil {
					toks := resep.Split(line, -1)
					targs := fillTmplMap(toks, line)
					_, err := tmpl.Execute(&buf, targs)
					check(err)
					handleCommand(args, buf.String(), ch)
				} else {
					lines = append(lines, line)
				}
			} else {
				if serr == io.EOF {
					break
				}
				log.Fatal(serr)
			}
			if len(lines) >= args.Nlines {
				targs := fillTmplMap(lines, strings.Join(lines, fs))
				_, err := tmpl.Execute(&buf, targs)
				check(err)
				lines = lines[:0]
				handleCommand(args, buf.String(), ch)
			}
		}
		if len(lines) > 0 {
			targs := fillTmplMap(lines, strings.Join(lines, fs))
			_, err := tmpl.Execute(&buf, targs)
			check(err)
			handleCommand(args, buf.String(), ch)
		}
		close(ch)
	}()
	return ch
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func init() {
	color.NoColor = !isatty.IsTerminal(os.Stderr.Fd())
}

func run(args Params) {

	tmpl := makeCommandTmpl(args.Command)
	cmds := genCommands(&args, tmpl)

	stdout := bufio.NewWriter(os.Stdout)
	defer stdout.Flush()

	cancel := make(chan bool)
	defer close(cancel)
	fails := 0

	// flush stdout every 2 seconds.
	last := time.Now().Add(2 * time.Second)
	for p := range process.Runner(cmds, args.Retry, cancel, nil, args.Ordered) {
		if ex := p.ExitCode(); ex != 0 {
			c := color.New(color.BgRed).Add(color.Bold)
			fmt.Fprintf(os.Stderr, "%s\n", c.SprintFunc()(fmt.Sprintf("ERROR with command: %s", p)))
			ExitCode = max(ExitCode, ex)
			fails++
			if args.StopOnError {
				break
			}
		}
		if args.Verbose {
			fmt.Fprintf(os.Stderr, "%s\n", p)
		}
		_, err := io.Copy(stdout, p)
		check(err)

		p.Cleanup()
		if t := time.Now(); t.After(last) {
			stdout.Flush()
			last = t.Add(2 * time.Second)
		}
		if args.log != nil {
			// if no error prefix the command with '#'
			if p.ExitCode() == 0 {
				args.log.WriteString("# " + strings.Replace(p.CmdStr, "\n", "\n# ", -1) + "\n")
			} else {
				args.log.WriteString(p.CmdStr + "\n")
			}
			stdout.Flush()
		}
	}
	stdout.Flush()
	if ExitCode == 0 && args.log != nil {
		args.log.WriteString("# SUCCESS\n")
	} else if args.log != nil {
		fmt.Fprintf(args.log, "# FAILED %d commands\n", fails)
	}

}

func makeCommandTmpl(cmd string) *fasttemplate.Template {
	v := strings.Replace(cmd, "{}", "{Line}", -1)
	return fasttemplate.New(v, "{", "}")
}
