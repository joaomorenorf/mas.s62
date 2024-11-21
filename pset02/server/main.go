package main

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log"
	"net"
	"os"
	"sort"
	"strings"
	"sync"
)

var (
	genesisBlock  = "0000000000000000000000000000000000000000000000000000000000000000 satoshi 11970128322"
	chainFilename = "./chain.txt"
)

// A hash is a sha256 hash, as in pset01
type Hash [32]byte

// ToString gives you a hex string of the hash
func (hs Hash) ToString() string {
	return fmt.Sprintf("%x", hs)
}

// Blocks are what make the chain in this pset; different than just a 32 byte array
// from last time.  Has a previous block hash, a name and a nonce.
type Block struct {
	PrevHash Hash
	Name     string
	Nonce    string
}

// ToString turns a block into an ascii string which can be sent over the
// network or printed to the screen.
func (bl Block) ToString() string {
	return fmt.Sprintf("%x %s %s", bl.PrevHash, bl.Name, bl.Nonce)
}

// Hash returns the sha256 hash of the block.  Hopefully starts with zeros!
func (bl Block) Hash() Hash {
	return sha256.Sum256([]byte(bl.ToString()))
}

// BlockFromString takes in a string and converts it to a block, if possible
func BlockFromString(s string) (Block, error) {
	var bl Block

	// check string length
	if len(s) < 66 || len(s) > 100 {
		return bl, fmt.Errorf("invalid string length %d, expect 66 to 100", len(s))
	}
	// split into 3 substrings via spaces
	subStrings := strings.Split(s, " ")

	if len(subStrings) != 3 {
		return bl, fmt.Errorf("got %d elements, expect 3", len(subStrings))
	}

	hashbytes, err := hex.DecodeString(subStrings[0])
	if err != nil {
		return bl, err
	}
	if len(hashbytes) != 32 {
		return bl, fmt.Errorf("got %d byte hash, expect 32", len(hashbytes))
	}

	copy(bl.PrevHash[:], hashbytes)

	bl.Name = subStrings[1]

	// remove trailing newline if there; the blocks don't include newlines, but
	// when transmitted over TCP there's a newline to signal end of block
	bl.Nonce = strings.TrimSpace(subStrings[2])

	// TODO add more checks on name/nonce ...?

	return bl, nil
}

// BlockChain is not actually a blockchain, it's just the tip.
// The chain itself only exists in a file.
type BlockChain struct {
	mtx   sync.Mutex
	tip   Block
	bchan chan Block
}

func Server() error {

	var bc BlockChain

	// initialize channel
	bc.bchan = make(chan Block, 8)

	// Ignore errors here; it's hard-coded
	// this is kindof ugly as it doesn't show up anywhere so it's like height
	// negative 1.  Weird but whatever.
	bc.tip, _ = BlockFromString(genesisBlock)

	// start handler routine for accepting new blocks from clients
	go HandleBlockSubmission(&bc)

	// handler is running, so feed it blocks from disk
	err := LoadChain(&bc)
	if err != nil {
		return err
	}

	serverListener, err := net.Listen("tcp", ":6262")
	if err != nil {
		return err
	}

	hiscoreListener, err := net.Listen("tcp", ":6299")
	if err != nil {
		return err
	}

	go ServeHiScores(hiscoreListener)

	for {
		// blocks here
		serverConnection, err := serverListener.Accept()
		if err != nil {
			return err
		}

		go HandleServerConnection(serverConnection, &bc)

	}
}

type Score struct {
	name   string
	points uint32
}
type ScoreList []Score

func (p ScoreList) Len() int           { return len(p) }
func (p ScoreList) Less(i, j int) bool { return p[i].points < p[j].points }
func (p ScoreList) Swap(i, j int)      { p[i], p[j] = p[j], p[i] }

// ServeHiScores listens for incoming TCP connections and sends them the high
// scores of the blockchain.
func ServeHiScores(l net.Listener) {
	for {
		hiCon, err := l.Accept()
		if err != nil {
			log.Printf("Hi score server error: %s\n", err.Error())
		}
		chain, err := os.ReadFile(chainFilename)
		if err != nil {
			log.Printf("Hi score server error: %s\n", err.Error())
		}

		lines := strings.Split(string(chain), "\n")

		scoreMap := make(map[string]uint32)

		// populate map with scores
		for _, line := range lines {
			// assume all block strings are good, so name is [1]
			vals := strings.Split(line, " ")
			if len(vals) < 3 {
				continue
			}
			_, valid := scoreMap[vals[1]]
			if !valid {
				scoreMap[vals[1]] = 1
			} else {
				scoreMap[vals[1]]++
			}
		}

		// make sortable score slice
		sl := make(ScoreList, len(scoreMap))
		// populate slice
		var i uint32
		for k, v := range scoreMap {
			sl[i] = Score{k, v}
			i++
		}
		// sort slice
		sort.Sort(sort.Reverse(sl))

		scoreReply := "--- pset02 high score list ---\n"
		scoreReply += "rank\tpoints\tname\n"
		// print in order to string
		for i, sc := range sl {
			scoreReply += fmt.Sprintf("#%d\t%d\t%s\n", i, sc.points, sc.name)
		}

		recentReply := "--- most recent blocks ---\n"
		for i := len(lines) - 10; i < len(lines); i++ {
			recentReply += lines[i] + "\n"
		}

		_, err = hiCon.Write([]byte(scoreReply + recentReply))
		if err != nil {
			log.Printf("Hi score server error: %s\n", err.Error())
		}
		hiCon.Close()
	}
}

// LoadChain reloads an old chain from disk
func LoadChain(bc *BlockChain) error {

	f, err := os.OpenFile(chainFilename, os.O_RDONLY, 0666)
	if err != nil {
		return err
	}

	scanner := bufio.NewScanner(f)

	for scanner.Scan() {
		blockLine := scanner.Text()

		newBl, err := BlockFromString(string(blockLine))
		if err != nil {
			return err
		}
		// submit block to handler routine
		bc.bchan <- newBl
	}
	return nil
}

/*
Server protocol: listen for command.  Commands are "TRQ" or a block.
Respond to TRQ with tip block.
Respond to block with ACK message accepting block hash, or error.
Respond to any other command with "Unknown command"
*/

// Handle the connection from clients.  Concurrent goroutine, so there
// can be a bunch of these happening at once
func HandleServerConnection(connection net.Conn, bc *BlockChain) {

	log.Printf("Incoming connection from %s\n", connection.RemoteAddr().String())

	// First, read from them
	bufReader := bufio.NewReader(connection)

	blockLine, err := bufReader.ReadBytes('\n')
	if err != nil {
		log.Printf("TCP error: %s\n", err.Error())
	}

	// sendBytes is whatever we're going to send them
	var sendBytes []byte

	// Now detect if it's a tip request
	if strings.HasPrefix(string(blockLine), "TRQ") {
		// ready tip for sending
		// lock mutex, get the string to send, and unlock
		bc.mtx.Lock()
		sendString := bc.tip.ToString()
		bc.mtx.Unlock()

		// use newline to indicate end of transmission.  A bit ugly but OK.
		sendBytes = []byte(fmt.Sprintf("%s\n", sendString))
	} else {
		// interpret as block submission
		newBl, err := BlockFromString(string(blockLine))
		if err != nil {
			// neither TRQ nor block, send error message
			sendBytes = []byte(fmt.Sprintf(
				"Malformed block error: %s\n", err.Error()))
		} else {
			// no error, absorb block

			// first check if it will append
			if !CheckNextBlock(bc.tip, newBl) {
				sendBytes = []byte(
					"Block invalid: not enough work, or doesn't connect to tip\n")
			} else {
				sendBytes = []byte(
					"Block accepted\n")
			}
			// submit block to handler routine
			bc.bchan <- newBl
		}
	}

	_, err = connection.Write(sendBytes)
	if err != nil {
		log.Printf("TCP error: %s\n", err.Error())
	}

	// disconnect client
	connection.Close()

}

// HandleBlockSubmission checks that the block is OK and fits on the end of the
// chain, then adds it on at the end.
func HandleBlockSubmission(bc *BlockChain) {
	var blockHistory []Block

	// loop forever
	for {
		proposedBlock := <-bc.bchan
		log.Printf("got hash %x\n", proposedBlock.Hash())

		// TODO actually check stuff

		// Pretty sure I don't need a mutex here because I'm just reading
		// and this function is the only thing that can write.  Also there's
		// only 1 iteration of this loop happening at a time.
		if !CheckNextBlock(bc.tip, proposedBlock) {
			log.Printf("Invalid block received:\n%s\n", proposedBlock.ToString())
			continue
		}

		// this thing keeps history, maybe save to disk...
		blockHistory = append(blockHistory, proposedBlock)

		// use mutex and update tip
		// also append to file
		bc.mtx.Lock()
		bc.tip = proposedBlock

		f, err := os.OpenFile(chainFilename, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0666)
		if err != nil {
			// crash if file doesn't work
			panic(err)
		}

		f.WriteString(proposedBlock.ToString() + "\n")
		f.Close()

		bc.mtx.Unlock()

		log.Printf("Block accepted; height is now %d\n", len(blockHistory))

	}
}

// CheckNextBlock checks if the block attaches, has work, etc.
// Assumes "prev" block is OK, but checks "next"
func CheckNextBlock(prev, next Block) bool {
	// first check the work on the new block.  33 bits needed.
	if !CheckWork(next, 33) {
		log.Printf("not enought work! ")
		return false
	}

	// first, check that next points back to prev
	if prev.Hash() != next.PrevHash {
		log.Printf("hashes don't chain up! ")
		return false
	}

	// probably enough checks for now?

	return true
}

// CheckWork checks if there's enough work
func CheckWork(bl Block, targetBits uint8) bool {
	h := bl.Hash()

	for i := uint8(0); i < targetBits; i++ {
		// for every bit from the MSB down, check if it's a 1.
		// If it is, stop and fail.
		// Could definitely speed this up by checking bytes at a time.
		// Left as excercise for the reader...?
		if (h[i/8]>>(7-(i%8)))&0x01 == 1 {
			return false
		}
	}
	return true
}

func main() {
	panic(Server())
}
