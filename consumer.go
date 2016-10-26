package funnel

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path"
	"sync"
	"syscall"
	"time"
)

// Consumer is the main struct which holds all the stuff
// necessary to run the code
type Consumer struct {
	Config        *Config
	LineProcessor LineProcessor

	// internal stuff
	currFile *os.File
	writer   *bufio.Writer
	feed     chan string

	// channel signallers
	done         chan struct{}
	rolloverChan chan struct{}
	signalChan   chan os.Signal
	wg           sync.WaitGroup

	// variable to track write progress
	linesWritten int
	bytesWritten uint64
}

// Start takes the input stream and begins reading line by line
// buffering the output to a file and flushing at set intervals
func (c *Consumer) Start(inputStream io.Reader) {
	c.setupSignalHandling()
	c.done = make(chan struct{})
	c.rolloverChan = make(chan struct{})

	// Make the dir along with parents
	if err := os.MkdirAll(c.Config.DirName, 0775); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return
	}

	// Create the file
	if err := c.createNewFile(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return
	}

	// Create the line feed channel and start the feed goroutine
	c.feed = make(chan string)
	go c.startFeed()

	// Get the reader to the input stream and set initial counters
	reader := bufio.NewReader(inputStream)
	c.linesWritten = 0
	c.bytesWritten = 0
	for {
		// This will return a line until delimiter
		// If delimiter is not found, it returns the line with error
		// so line will always be available
		// Then we check for error and quit
		line, err := reader.ReadString('\n')

		// Send to feed
		c.feed <- line

		// Update counters
		c.linesWritten++
		c.bytesWritten += uint64(len(line))

		// Check for rollover
		if c.rollOverCondition() {
			c.rolloverChan <- struct{}{}
			c.linesWritten = 0
			c.bytesWritten = 0
		}

		if err != nil {
			if err != io.EOF {
				fmt.Fprintln(os.Stderr, err)
			}
			break
		}
	}
	// work is done, signalling done channel
	c.wg.Add(1)
	c.done <- struct{}{}
	c.wg.Wait()
	// quitting from signal handler
	close(c.signalChan)
}

func (c *Consumer) cleanUp() {
	// Close file handle
	if err := c.currFile.Sync(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return
	}

	if err := c.currFile.Close(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return
	}

	// Rename the currfile to a rolled up one
	if err := c.renameAndCompress(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return
	}
}

func (c *Consumer) createNewFile() error {
	f, err := os.OpenFile(path.Join(c.Config.DirName, c.Config.ActiveFileName),
		os.O_CREATE|os.O_WRONLY|os.O_TRUNC|os.O_APPEND|os.O_EXCL,
		0644)
	if err != nil {
		return err
	}
	c.currFile = f
	c.writer = bufio.NewWriter(c.currFile)
	return nil
}

func (c *Consumer) rollOverCondition() bool {
	// Return true if either lines written has exceeded
	// or bytes written has exceeded
	return c.linesWritten >= c.Config.RotationMaxLines ||
		c.bytesWritten >= c.Config.RotationMaxBytes
}

func (c *Consumer) rollOver() error {
	// Flush writer
	if err := c.writer.Flush(); err != nil {
		return err
	}

	// Close file handle
	if err := c.currFile.Sync(); err != nil {
		return err
	}
	if err := c.currFile.Close(); err != nil {
		return err
	}

	if err := c.renameAndCompress(); err != nil {
		return err
	}

	// XXX: check if there are any files to delete

	if err := c.createNewFile(); err != nil {
		return err
	}
	return nil
}

func (c *Consumer) renameAndCompress() error {
	var fileName string
	var err error
	if c.Config.FileRenamePolicy == "timestamp" {
		err, fileName = renameFileTimestamp(c.Config)
		if err != nil {
			return err
		}
	} else {
		err, fileName = renameFileSerial(c.Config)
		if err != nil {
			return err
		}
	}
	if c.Config.Gzip {
		if err := gzipFile(path.Join(c.Config.DirName, fileName)); err != nil {
			return err
		}
	}
	return nil
}

func (c *Consumer) startFeed() {
	// Will flush the writer at some intervals
	ticker := time.NewTicker(time.Duration(c.Config.FlushingTimeIntervalSecs) * time.Second)
	for {
		select {
		case line := <-c.feed: // Write to buffered writer
			err := c.LineProcessor.Write(c.writer, line)
			if err != nil {
				fmt.Fprintln(os.Stderr, err)
				return
			}
		case <-c.rolloverChan: // Rollover file to new one
			if err := c.rollOver(); err != nil {
				fmt.Fprintln(os.Stderr, err)
				return
			}
		case <-c.done: // Done signal received, close shop
			ticker.Stop()
			if err := c.writer.Flush(); err != nil {
				fmt.Fprintln(os.Stderr, err)
			}
			c.cleanUp()
			c.wg.Done()
			return
		case <-ticker.C: // If tick happens, flush the writer
			if err := c.writer.Flush(); err != nil {
				fmt.Fprintln(os.Stderr, err)
			}
		}
	}
}

func (c *Consumer) setupSignalHandling() {
	c.signalChan = make(chan os.Signal, 1)
	signal.Notify(c.signalChan,
		os.Interrupt, syscall.SIGPIPE)

	// Block until a signal is received.
	// Or EOF happens
	go func() {
		for range c.signalChan {
		}
	}()
}
