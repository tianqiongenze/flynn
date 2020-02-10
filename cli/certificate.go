package main

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"os"
	"strings"

	"github.com/flynn/flynn/controller/api"
	controller "github.com/flynn/flynn/controller/client"
	"github.com/flynn/go-docopt"
	"github.com/olekukonko/tablewriter"
	"github.com/smallstep/certinfo"
)

func init() {
	register("certificate", runCertificate, `
usage: flynn certificate
       flynn certificate add [--ref=<ref>] [--meta=<key>=<val>...] <cert> <key>
       flynn certificate remove <name>
       flynn certificate inspect <name>

Manage static TLS certificates.

Commands:
	With no arguments, shows a list of certificates.

	add     adds a certificate
	remove  removes a certificate
	inspect prints information about a certificate

Arguments:
	<cert>  path to PEM encoded certificate, - for stdin
	<key>   path to PEM encoded private key, - for stdin
	<name>	name of the certificate

Options:
	-r, --ref=<ref>         human readable reference (e.g. 'my-cert-2019-v1') - must be unique
	-m, --meta=<key>=<val>  metadata to add to the certificate
`)
}

func runCertificate(args *docopt.Args, client controller.Client) error {
	if args.Bool["add"] {
		return runCertificateAdd(args, client)
	} else if args.Bool["remove"] {
		return runCertificateRemove(args, client)
	} else if args.Bool["inspect"] {
		return runCertificateInspect(args, client)
	}

	var req api.ListCertificatesRequest
	var res api.ListCertificatesResponse
	if err := client.Invoke("flynn.api.v1.Router/ListCertificates", &req, &res); err != nil {
		return err
	}

	table := tablewriter.NewWriter(os.Stdout)
	table.SetRowLine(true)
	table.SetAutoWrapText(false)
	table.SetHeader([]string{"REF", "ROUTES", "DOMAINS", "META"})
	defer table.Render()
	for _, cert := range res.Certificates {
		var domains []string
		if v, ok := cert.Certificate.(*api.Certificate_Static); ok {
			domains = v.Static.Domains
		}
		meta := make([]string, 0, len(cert.Meta))
		for k, v := range cert.Meta {
			meta = append(meta, fmt.Sprintf("%s=%s", k, v))
		}
		table.Append([]string{
			cert.Name,
			strings.Join(cert.Routes, "\n"),
			strings.Join(domains, "\n"),
			strings.Join(meta, "\n"),
		})
	}
	return nil
}

func runCertificateAdd(args *docopt.Args, client controller.Client) error {
	certPEM, keyPEM, err := parseTLSCert(args.String["<cert>"], args.String["<key>"])
	if err != nil {
		return fmt.Errorf("error loading certificate and key: %s", err)
	}
	cert, err := tls.X509KeyPair([]byte(certPEM), []byte(keyPEM))
	if err != nil {
		return fmt.Errorf("error loading certificate and key: %s", err)
	}
	key, err := x509.MarshalPKCS8PrivateKey(cert.PrivateKey)
	if err != nil {
		return fmt.Errorf("error encoding private key: %s", err)
	}
	metaArgs := args.All["--meta"].([]string)
	meta := make(map[string]string, len(metaArgs))
	for _, m := range metaArgs {
		keyVal := strings.SplitN(m, "=", 2)
		if len(keyVal) != 2 {
			return fmt.Errorf("invalid meta value: %q", m)
		}
		meta[keyVal[0]] = keyVal[1]
	}
	req := api.CreateCertificateRequest{
		Certificate: &api.CreateCertificateRequest_Static_{Static: &api.CreateCertificateRequest_Static{
			Chain:      cert.Certificate,
			PrivateKey: key,
		}},
		Ref:  args.String["--ref"],
		Meta: meta,
	}
	var res api.CreateCertificateResponse
	if err := client.Invoke("flynn.api.v1.Router/CreateCertificate", &req, &res); err != nil {
		return err
	}
	fmt.Println(res.Certificate.Name)
	return nil
}

func runCertificateRemove(args *docopt.Args, client controller.Client) error {
	req := api.DeleteCertificateRequest{
		Name: args.String["<name>"],
	}
	var res api.DeleteCertificateResponse
	if err := client.Invoke("flynn.api.v1.Router/DeleteCertificate", &req, &res); err != nil {
		return err
	}
	fmt.Printf("Certificate %s removed.\n", res.Certificate.Name)
	return nil
}

func runCertificateInspect(args *docopt.Args, client controller.Client) error {
	req := api.GetCertificateRequest{
		Name: args.String["<name>"],
	}
	var res api.GetCertificateResponse
	if err := client.Invoke("flynn.api.v1.Router/GetCertificate", &req, &res); err != nil {
		return err
	}
	staticCert, ok := res.Certificate.Certificate.(*api.Certificate_Static)
	if !ok {
		return fmt.Errorf("unexpected certificate type: %T", res.Certificate.Certificate)
	}
	certs, err := x509.ParseCertificates(bytes.Join(staticCert.Static.Chain, []byte{}))
	if err != nil {
		return err
	}
	for _, cert := range certs {
		text, err := certinfo.CertificateText(cert)
		if err != nil {
			return err
		}
		fmt.Println(text)
	}
	return nil
}
