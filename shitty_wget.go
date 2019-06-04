package main

import (
	"bufio"
	"flag"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// FileNameList is a list of filenames.
//
// It implements the flag.Value interface for use with the flag package.
type FileNameList []string

func (l *FileNameList) String() string {
	return strings.Join(*l, ",")
}

func (l *FileNameList) Set(fName string) error {
	*l = append(*l, fName)
	return nil
}

// User agents avaiable
var UserAgents = []string{
	"Mozilla/5.0 (X11; Linux i686; rv:64.0) Gecko/20100101 Firefox/64.0",
	"Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/70.0.3538.77 Safari/537.36",
	"Mozilla/5.0 (Windows NT 6.1; WOW64; Trident/7.0; AS; rv:11.0) like Gecko"}

// Flag variables

var connections = flag.Int("c", 1, "number of `connections`, or files downloaded concurrently; numbers <= 0 will be interpreted as 1")
var overwriteExisting = flag.Bool("over", false, "overwrite existing file with the same name already in the filesystem; otherwise, will try to make an unique name")
var destDir = flag.String("dest", ".", "destination `directory` for downloaded files")
var randomUserAgent = flag.Bool("random-agent", false, "randomnize reported user agent string; can help with bot blocking")
var listUserAgents = flag.Bool("list-agents", false, "list internally avaiable User-Agent strings and exit")
var customAgent = flag.String("custom-agent", "", "set a custom `User-Agent` string; overrides random-agent")
var waitTime = flag.Int("wait", 0, "wait an amount of `seconds` between individual connections; numbers < 0 will be interpreted as 0; can be used in conjunction with random-wait")
var randomWait = flag.Bool("random-wait", false, "instead of waiting a fixed amount of time, wait a random amount of seconds between 0 to the number specified by wait; not much will happen if wait is not specified")
var urlFiles FileNameList

func main() {
	// Flags setup
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "use: %[1]s [OPTIONS] [URL [URL2...]]\n\n", filepath.Base(os.Args[0]))
		fmt.Fprintf(os.Stderr, "Download from a list of URLs, either passed directly from the command line or from a file. Can optionally make many downloads concurrently\n\n")
		flag.PrintDefaults()
	}
	flag.Var(&urlFiles, "i", "add an input `file` containing one URL per line; can be used multiple times")
	flag.Parse()

	// Normilize certain variables
	if *connections <= 0 {
		*connections = 1
	}
	if *waitTime < 0 {
		*waitTime = 0
	}

	if *listUserAgents {
		fmt.Println(strings.Join(UserAgents, "\n"))
		os.Exit(0)
	}

	// If invoked with no arguments, consider it a valid way to ask for help
	if len(urlFiles) == 0 && len(flag.Args()) == 0 {
		flag.Usage()
		os.Exit(0)
	}

	rand.Seed(time.Now().Unix())

	if *randomUserAgent && *customAgent == "" {
		*customAgent = UserAgents[rand.Intn(len(UserAgents))]
		fmt.Fprintf(os.Stderr, "used user-agent: %s\n", *customAgent)
	}

	// Define a standard function to wait a certain amount of time
	waitFunction := simpleWaitFunc

	if *randomWait && *waitTime > 0 {
		waitFunction = randomWaitFunc
	}

	// List of io.Readers to read URLs from. Initial length is 0; will append as
	// necessary, ignoring errors and attempting to continue. The cap takes into account
	// each reader, the newline that's always added at the end,
	// and the args plus it's newline
	urlReaders := make([]io.Reader, 0, (len(urlFiles)*2)+len(flag.Args()))

	for _, fName := range urlFiles {
		f, err := os.Open(fName)
		if err != nil {
			fmt.Fprintf(os.Stderr, "error opening %s: %v\n", fName, err)
		}
		defer f.Close() // Close at the end of the main program

		// Make sure there's always a newline next
		urlReaders = append(urlReaders, f, strings.NewReader("\n"))
	}

	// Append last "file", made by all urls passed with the command line
	if len(flag.Args()) > 0 {
		urlReaders = append(urlReaders, strings.NewReader(strings.Join(flag.Args(), "\n")+"\n"))
	}

	httpClient := &http.Client{}

	semaphore := make(chan struct{}, *connections)
	var wg sync.WaitGroup

	if *destDir != "." {
		if !fileExists(*destDir) {
			err := os.MkdirAll(*destDir, os.ModePerm)
			if err != nil {
				fmt.Fprintf(os.Stderr, "error creating new destination directory %s: %v", *destDir, err)
				os.Exit(1)
			}
		}

		err := os.Chdir(*destDir)
		if err != nil {
			fmt.Fprintf(os.Stderr, "error while chaning working directory to %s: %v", *destDir, err)
			os.Exit(1)
		}
	}

	urlScanner := bufio.NewReader(io.MultiReader(urlReaders...))
	firstLoop := true
	for {
		// This is a serial part, which should be done in the main thread

		line, err := urlScanner.ReadString('\n')
		if err != nil {
			// Gneral unexpected errors
			if err != io.EOF {
				fmt.Fprintf(os.Stderr, "error while scanning lines: %v\n", err)
			}

			// We received an EOF. Because of initial program setup, the scanner
			// should end in newline and, therefore, has no more data to consume.
			// Can break loop without worries
			break
		}

		if !firstLoop {
			waitFunction(*waitTime)
		} else {
			firstLoop = false
		}

		url := strings.TrimSpace(line)
		// Skip empty lines
		if url == "" {
			continue
		}

		wg.Add(1)
		go func(url string) {
			semaphore <- struct{}{}
			// Liberate semaphore and decrement waitgroup just before leaving
			defer func() { <-semaphore }()
			defer wg.Done()

			req, errWorker := http.NewRequest("GET", url, nil)
			if errWorker != nil {
				fmt.Fprintf(os.Stderr, "error creating request for %s: %v\n", url, errWorker)
				return
			}

			if *customAgent != "" {
				req.Header.Set("User-Agent", *customAgent)
			}

			resp, errWorker := httpClient.Do(req)
			if errWorker != nil {
				fmt.Fprintf(os.Stderr, "error downloading %s: %v\n", url, errWorker)
				return
			}
			defer resp.Body.Close()

			filename := getNameFromUrl(url, !*overwriteExisting)

			f, errWorker := os.Create(filename)
			if errWorker != nil {
				fmt.Fprintf(os.Stderr, "error creating file %s: %v\n", filename, errWorker)
				return
			}
			defer f.Close()

			filename, errWorker = filepath.Abs(filename)
			if errWorker != nil {
				fmt.Fprintf(os.Stderr, "error while getting absolute path of %s: %v\n", filename, errWorker)
				return
			}

			_, errWorker = io.Copy(f, resp.Body)
			if errWorker != nil {
				fmt.Fprintf(os.Stderr, "error during download %s to %s: %v\n", url, filename, errWorker)
				return
			}

			fmt.Fprintf(os.Stderr, "%s\n -> %s\n", url, filename)
		}(url)
	}

	wg.Wait()

}

// simpleWaitFunc waits a given amount of time in seconds
func simpleWaitFunc(waitTime int) {
	time.Sleep(time.Duration(waitTime) * time.Second)
}

// randomWait waits a random amount of time, from 0 to waitTime seconds.
//
// Will panic if waitTime < 0
func randomWaitFunc(waitTime int) {
	time.Sleep(time.Duration(rand.Intn(waitTime+1)) * time.Second)
}

// fileExists is a utility function that checks if  file exists
func fileExists(fileName string) bool {
	_, err := os.Stat(fileName)
	if os.IsNotExist(err) {
		return false
	}

	return true
}

// getNameFromUrl takes an url and fabricates a suitable name from it.
//
// Optionally, can try to create a unique filename if it detects the file already exists,
// to avoid overwriting
func getNameFromUrl(url string, detectRepeatedNames bool) string {
	baseName := strings.Split(path.Base(url), "?")[0]

	currentName := baseName

	// If name is empty, use a timestampt
	if currentName == "" {
		currentName = time.Now().Format(time.RFC3339)
	}

	if detectRepeatedNames {
		ext := filepath.Ext(baseName)
		baseNoExt := strings.TrimSuffix(baseName, ext)
		for counter := 1; fileExists(currentName); counter++ {
			currentName = fmt.Sprintf("%s-%d%s", baseNoExt, counter, ext)
		}
	}

	return currentName
}
