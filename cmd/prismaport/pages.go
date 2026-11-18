package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"path/filepath"

	"github.com/prismaport/prismaport/internal/api"
	checkoutweb "github.com/prismaport/prismaport/web/checkout"
	dashboardweb "github.com/prismaport/prismaport/web/dashboard"
	docsweb "github.com/prismaport/prismaport/web/docs"
	landingweb "github.com/prismaport/prismaport/web/landing"
)

// embeddedPages are the pages this binary serves in embedded mode, which the Pages build copies.
func embeddedPages() api.PagesSources {
	return api.PagesSources{Landing: landingweb.Site(), Dashboard: dashboardweb.Dist(), Checkout: checkoutweb.Dist(), Docs: docsweb.Dist()}
}

// runPages builds the Cloudflare Pages directories of split mode
// from the pages embedded in this binary.
func runPages(args []string, out io.Writer, src api.PagesSources) error {
	if len(args) == 0 || args[0] != "build" {
		return errors.New("usage: prismaport pages build --out DIR --site-url URL --app-url URL --pay-url URL --docs-url URL --api-url URL [--noindex]")
	}
	fs := flag.NewFlagSet("pages build", flag.ContinueOnError)
	fs.SetOutput(out)
	dir := fs.String("out", "", "directory to write site/, app/, pay/ and docs/ into (those four are replaced)")
	var o api.PagesOrigins
	fs.StringVar(&o.Site, "site-url", "", "the landing page's origin, e.g. https://prismaport.com")
	fs.StringVar(&o.App, "app-url", "", "the dashboard's origin, e.g. https://app.prismaport.com (served at /dashboard/)")
	fs.StringVar(&o.Pay, "pay-url", "", "the hosted checkout's origin, e.g. https://pay.prismaport.com")
	fs.StringVar(&o.Docs, "docs-url", "", "the developer portal's origin, e.g. https://docs.prismaport.com (served at its root)")
	fs.StringVar(&o.API, "api-url", "", "the API's origin, e.g. https://api.prismaport.com")
	fs.BoolVar(&o.NoIndex, "noindex", false, "send X-Robots-Tag: noindex, nofollow on every page (a non-production environment)")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("unexpected argument %q", fs.Arg(0))
	}
	if *dir == "" {
		return errors.New("--out is required")
	}
	if err := api.BuildPages(*dir, o, src); err != nil {
		return err
	}
	for _, d := range []string{api.PagesSiteDir, api.PagesAppDir, api.PagesPayDir, api.PagesDocsDir} {
		fmt.Fprintln(out, filepath.Join(*dir, d))
	}
	return nil
}
