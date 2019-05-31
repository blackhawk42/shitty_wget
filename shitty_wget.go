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
var listUserAgents = flag.Bool("list-agents", false, "list avaiable User-Agent strings and exit")
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

	// Make sure number of connections is setup to sumething reasonable
	if *connections <= 0 {
		*connections = 1
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

	customAgent := ""
	if *randomUserAgent {
		rand.Seed(time.Now().Unix())
		customAgent = UserAgents[rand.Intn(len(UserAgents))]
		fmt.Fprintf(os.Stderr, "used user-agent: %s\n", customAgent)
	}

	// List of io.Readers to read URLs from. Initial length is 0; will append as
	// necessary, ignoring errors and attempting to continue. The cap takes into account
	// each reader, the newline that's always added at the end,
	// and the args
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
		urlReaders = append(urlReaders, strings.NewReader(strings.Join(flag.Args(), "\n")))
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

	urlScanner := bufio.NewScanner(io.MultiReader(urlReaders...))
	for urlScanner.Scan() {
		// This is a serial part, which should be done in the main thread
		url := urlScanner.Text()

		// Skip empty lines
		if strings.TrimSpace(url) == "" {
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

			if *randomUserAgent {
				req.Header.Set("User-Agent", customAgent)
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
				fmt.Fprintf(os.Stderr, "error while getting absolute path of %s: %v", filename, errWorker)
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
	baseName := path.Base(url)

	currentName := baseName

	if detectRepeatedNames {
		ext := filepath.Ext(baseName)
		baseNoExt := strings.TrimSuffix(baseName, ext)
		for counter := 1; fileExists(currentName); counter++ {
			currentName = fmt.Sprintf("%s-%d%s", baseNoExt, counter, ext)
		}
	}

	return currentName
}
