package main

import (
	"bytes"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"io"
	"io/ioutil"
	"math/big"
	"net"
	"os"
	"time"

	"github.com/immesys/bw2/crypto"
	"github.com/immesys/bw2/objects"
	"github.com/immesys/bw2bind"
)

func genCert(vk string) (tls.Certificate, *x509.Certificate) {
	serialNumberLimit := new(big.Int).Lsh(big.NewInt(1), 128)
	serialNumber, err := rand.Int(rand.Reader, serialNumberLimit)
	if err != nil {
		panic(err)
	}
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		panic(err)
	}
	template := x509.Certificate{
		SerialNumber: serialNumber,
		Subject: pkix.Name{
			CommonName: vk,
		},
		NotBefore: time.Now(),
		NotAfter:  time.Now().Add(365 * 24 * time.Hour),

		KeyUsage:              x509.KeyUsageKeyEncipherment | x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
	}
	template.IsCA = true
	template.KeyUsage |= x509.KeyUsageCertSign
	derBytes, err := x509.CreateCertificate(rand.Reader, &template, &template, &priv.PublicKey, priv)
	if err != nil {
		panic(err)
	}
	x509cert, err := x509.ParseCertificate(derBytes)
	if err != nil {
		panic(err)
	}

	keybytes := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(priv)})
	certbytes := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: derBytes})
	cert, err := tls.X509KeyPair(certbytes, keybytes)
	if err != nil {
		panic(err)
	}
	return cert, x509cert
}

var ourEntity *objects.Entity

func doserver(serverEntityFile string, listenAddr string, agentaddr string) {
	cl := bw2bind.ConnectOrExit(agentaddr)
	econtents, err := ioutil.ReadFile(serverEntityFile)
	if err != nil {
		panic(err)
	}
	enti, err := objects.NewEntity(objects.ROEntityWKey, econtents[1:])
	if err != nil {
		panic(err)
	}
	cl.SetEntity(econtents[1:])
	ourEntity = enti.(*objects.Entity)
	cert, cert2 := genCert(crypto.FmtKey(ourEntity.GetVK()))
	tlsConfig := tls.Config{Certificates: []tls.Certificate{cert}}
	ln, err := tls.Listen("tcp", listenAddr, &tlsConfig)
	fmt.Printf("ragent listening on: %s\n", listenAddr)
	if err != nil {
		fmt.Printf("Could not open native adapter socket: %v\n", err)
		os.Exit(1)
	}
	proof := make([]byte, 32+64)
	copy(proof, ourEntity.GetVK())
	if err != nil {
		fmt.Printf("Could not parse certificate\n")
		os.Exit(1)
	}
	crypto.SignBlob(ourEntity.GetSK(), ourEntity.GetVK(), proof[32:], cert2.Raw)
	for {
		conn, err := ln.Accept()
		fmt.Println("accepted connection from", conn.RemoteAddr())
		if err != nil {
			panic(err)
		}
		//First thing we do is write the 96 byte proof that the self-signed cert was
		//generated by the person posessing the router's SK
		conn.Write(proof)
		//Then handle the session
		go handleSession(conn, cl, agentaddr)
	}
}

func handleSession(conn net.Conn, globalcl *bw2bind.BW2Client, agentaddr string) {
	//At this stage, the remote side trusts that we are a specific entity.
	//Now, we send them a nonce and they send back their VK, and a signature of the nonce
	//Then we look for a DOT from our VK to their VK with the appropriate URI
	nonce := make([]byte, 32)
	_, err := rand.Read(nonce)
	if err != nil {
		panic(err)
	}
	conn.Write(nonce)
	reply := make([]byte, 32+64)
	_, err = io.ReadFull(conn, reply)
	if err != nil {
		panic(err)
	}
	vk := reply[:32]
	sig := reply[32:]
	if !crypto.VerifyBlob(vk, sig, nonce) {
		panic("client sig invalid")
	}
	fmt.Println("client signature valid")
	//Now try find that dot
	ok := isVKAllowed(vk, globalcl)
	if !ok {
		conn.Write([]byte("FAIL"))
		conn.Close()
		panic("client is not permitted to use this ragent")
	}
	fmt.Println("ragent permission dot exists for client ", crypto.FmtKey(vk))
	//Client is permitted
	conn.Write([]byte("OKAY"))
	//Simply relay from now on:
	acon, err := net.Dial("tcp", agentaddr)
	if err != nil {
		panic(err)
	}
	fmt.Println("beginning relay: ", conn.RemoteAddr())
	go copysimplex("local->remote", acon, conn)
	copysimplex("remote->local", conn, acon)

	fmt.Println("relay terminated: ", conn.RemoteAddr())
	acon.Close()
	conn.Close()
}

func copysimplex(desc string, a, b net.Conn) {
	total := 0
	last := time.Now()
	buf := make([]byte, 4096)
	for {
		count, err := a.Read(buf)
		if count == 0 || err != nil {
			break
		}
		b.Write(buf[:count])
		total += count
		if time.Now().Sub(last) > 5*time.Second {
			fmt.Println(desc, total, "bytes")
		}
	}
}

func isVKAllowed(vk []byte, cl *bw2bind.BW2Client) bool {
	dots, validities, err := cl.FindDOTsFromVK(crypto.FmtKey(ourEntity.GetVK()))
	if err != nil {
		panic(err)
	}
	everyone, _ := crypto.UnFmtKey("----EqP__WY477nofMYUz2MNFBsfa5IK_RBlRvKptDY=")
	for idx, d := range dots {
		if validities[idx] != bw2bind.StateValid {
			continue
		}
		if (bytes.Equal(d.GetReceiverVK(), vk) ||
			bytes.Equal(d.GetReceiverVK(), everyone)) && d.GetAccessURISuffix() == "1.0/full" {
			return true
		}
	}
	return false
}
