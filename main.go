package main

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime/pprof"
	"strings"
	"time"

	cli "gx/ipfs/QmVahSzvB3Upf5dAW15dpktF6PXb4z9V5LohmbcUqktyF4/cli"

	files "gx/ipfs/QmQ3zzxvxdX2YGogDpx23YHKRZ4rmqGoXmnoJNdwzxtkhc/go-ipfs/commands/files"
	core "gx/ipfs/QmQ3zzxvxdX2YGogDpx23YHKRZ4rmqGoXmnoJNdwzxtkhc/go-ipfs/core"
	cu "gx/ipfs/QmQ3zzxvxdX2YGogDpx23YHKRZ4rmqGoXmnoJNdwzxtkhc/go-ipfs/core/coreunix"
	bitswap "gx/ipfs/QmQ3zzxvxdX2YGogDpx23YHKRZ4rmqGoXmnoJNdwzxtkhc/go-ipfs/exchange/bitswap"
	filestore "gx/ipfs/QmQ3zzxvxdX2YGogDpx23YHKRZ4rmqGoXmnoJNdwzxtkhc/go-ipfs/filestore"
	balanced "gx/ipfs/QmQ3zzxvxdX2YGogDpx23YHKRZ4rmqGoXmnoJNdwzxtkhc/go-ipfs/importer/balanced"
	chunk "gx/ipfs/QmQ3zzxvxdX2YGogDpx23YHKRZ4rmqGoXmnoJNdwzxtkhc/go-ipfs/importer/chunk"
	h "gx/ipfs/QmQ3zzxvxdX2YGogDpx23YHKRZ4rmqGoXmnoJNdwzxtkhc/go-ipfs/importer/helpers"
	dag "gx/ipfs/QmQ3zzxvxdX2YGogDpx23YHKRZ4rmqGoXmnoJNdwzxtkhc/go-ipfs/merkledag"
	fsrepo "gx/ipfs/QmQ3zzxvxdX2YGogDpx23YHKRZ4rmqGoXmnoJNdwzxtkhc/go-ipfs/repo/fsrepo"
	ft "gx/ipfs/QmQ3zzxvxdX2YGogDpx23YHKRZ4rmqGoXmnoJNdwzxtkhc/go-ipfs/unixfs"

	human "gx/ipfs/QmPSBJL4momYnE7DcUyk2DVhD6rH488ZmHBGLbxNdhU44K/go-humanize"
	node "gx/ipfs/QmRSU5EqqWVZSNdbU51yXmVoF1uNw3JgTNB6RaiL7DZM16/go-ipld-node"
	ds "gx/ipfs/QmRWDav6mzWseLWeYfVd5fvUKiVe9xNH29YfMF438fG364/go-datastore"
)

var (
	cwd string
)

func init() {
	d, err := os.Getwd()
	if err != nil {
		fmt.Println(err)
		os.Exit(1)
	}
	cwd = d
}

const (
	ManifestFilename = "PackManifest"
	PackRepo         = ".ipfs-pack"
)

func main() {
	if err := doMain(); err != nil {
		fmt.Println(err)
		os.Exit(1)
	}
}

func doMain() error {
	proffi := os.Getenv("IPFS_PACK_PROFILE")
	if proffi != "" {
		fi, err := os.Create(proffi)
		if err != nil {
			return err
		}

		defer fi.Close()
		err = pprof.StartCPUProfile(fi)
		if err != nil {
			return err
		}

		defer pprof.StopCPUProfile()
	}

	app := cli.NewApp()
	app.Usage = "A filesystem packing tool."
	app.Version = "v0.1.0"
	app.Commands = []cli.Command{
		makePackCommand,
		verifyPackCommand,
		repoCommand,
		servePackCommand,
	}

	return app.Run(os.Args)
}

var makePackCommand = cli.Command{
	Name:      "make",
	Usage:     "makes the package, overwriting the PackManifest file.",
	ArgsUsage: "<dir>",
	Flags: []cli.Flag{
		cli.BoolFlag{
			Name:  "verbose",
			Usage: "enable verbose output.",
		},
	},
	Action: func(c *cli.Context) error {
		workdir := cwd
		if c.Args().Present() {
			argpath, err := filepath.Abs(c.Args().First())
			if err != nil {
				return err
			}
			workdir = argpath
		}

		verbose := c.Bool("verbose")
		repo, err := getRepo(workdir)
		if err != nil {
			return err
		}

		adder, err := getAdder(repo.Datastore(), repo.FileManager())
		if err != nil {
			return err
		}
		dirname := filepath.Base(workdir)

		output := make(chan interface{})
		adder.Out = output

		done := make(chan struct{})
		manifest, err := os.Create(filepath.Join(workdir, ManifestFilename))
		if err != nil {
			return err
		}

		imp := DefaultImporterSettings.String()

		go func() {
			defer close(done)
			defer manifest.Close()
			var count int
			for v := range output {
				count++
				ao := v.(*cu.AddedObject)
				towrite := ao.Name[len(dirname):]
				if len(towrite) > 0 {
					towrite = towrite[1:]
				} else {
					towrite = "."
				}
				fmt.Fprintf(manifest, "%s\t%s\t%s\n", ao.Hash, imp, towrite)
				if verbose {
					fmt.Printf("                                       \r")
					fmt.Printf("Processed %d files...\r", count)
				}
			}
		}()

		sf, err := getFilteredDirFile(workdir)
		if err != nil {
			return err
		}

		err = adder.AddFile(sf)
		if err != nil {
			return err
		}

		_, err = adder.Finalize()
		if err != nil {
			return err
		}

		close(output)
		<-done
		fmt.Println("wrote PackManifest")

		return nil
	},
}

var servePackCommand = cli.Command{
	Name:  "serve",
	Usage: "start an ipfs node to serve this pack's contents.",
	Flags: []cli.Flag{
		cli.BoolTFlag{
			Name:  "verify",
			Usage: "verify integrity of pack before serving.",
		},
	},
	Action: func(c *cli.Context) error {
		workdir := cwd
		if c.Args().Present() {
			argpath, err := filepath.Abs(c.Args().First())
			if err != nil {
				return err
			}
			workdir = argpath
		}

		packpath := filepath.Join(workdir, PackRepo)
		if !fsrepo.IsInitialized(packpath) {
			return fmt.Errorf("TODO: autogen repo on serve")
		}

		r, err := getRepo(workdir)
		if err != nil {
			return fmt.Errorf("error opening repo: %s", err)
		}

		verify := c.BoolT("verify")
		if verify {
			_, ds := buildDagserv(r.Datastore(), r.FileManager())
			fi, err := os.Open(filepath.Join(workdir, ManifestFilename))
			if err != nil {
				switch {
				case os.IsNotExist(err):
					return fmt.Errorf("error: no %s found in %s", ManifestFilename, workdir)
				default:
					return fmt.Errorf("error opening %s: %s", ManifestFilename, err)
				}
			}
			defer fi.Close()

			fmt.Println("Verifying pack contents before serving...")

			problem, err := verifyPack(ds, workdir, fi)
			if err != nil {
				return fmt.Errorf("error verifying pack: %s", err)
			}

			if problem {
				return fmt.Errorf("Pack verify failed, refusing to serve.\n  To continue, Fix the files contents and re-run 'ipfs-pack serve'\n  If these changes were intentional, re-run 'ipfs-pack make' to regenerate the manifest")
			} else {
				fmt.Println("Verified pack, starting server...")
			}
		}

		root, err := getManifestRoot(workdir)
		if err != nil {
			return err
		}

		cfg := &core.BuildCfg{
			Online: true,
			Repo:   r,
		}

		nd, err := core.NewNode(context.Background(), cfg)
		if err != nil {
			return err
		}
		fmt.Println("Serving data in this pack to the network...")
		fmt.Printf("Peer Multiaddrs:\n")
		for _, a := range nd.PeerHost.Addrs() {
			fmt.Printf("    %s/ipfs/%s\n", a, nd.Identity.Pretty())
		}

		fmt.Printf("\nPack root hash is /ipfs/%s\n\n", root)
		tick := time.NewTicker(time.Second * 2)
		for {
			select {
			case <-nd.Context().Done():
				return nil
			case <-tick.C:
				npeers := len(nd.PeerHost.Network().Peers())
				st, err := nd.Exchange.(*bitswap.Bitswap).Stat()
				if err != nil {
					fmt.Println("error getting block stat: ", err)
					continue
				}
				fmt.Printf(strings.Repeat("    ", 12) + "\r")
				fmt.Printf("Peers: %4d. Shared: %6d blocks, %s total data uploaded.   \r", npeers, st.BlocksSent, human.Bytes(st.DataSent))
			}
		}

		return nil
	},
}

var verifyPackCommand = cli.Command{
	Name:  "verify",
	Usage: "verifies the ipfs-pack manifest file is correct.",
	Action: func(c *cli.Context) error {
		workdir := cwd
		if c.Args().Present() {
			argpath, err := filepath.Abs(c.Args().First())
			if err != nil {
				return err
			}
			workdir = argpath
		}

		// TODO: check for files in pack that arent in manifest
		fi, err := os.Open(filepath.Join(workdir, ManifestFilename))
		if err != nil {
			switch {
			case os.IsNotExist(err):
				return fmt.Errorf("error: no %s found in %s", ManifestFilename, workdir)
			default:
				return fmt.Errorf("error opening %s: %s", ManifestFilename, err)
			}
		}

		var dstore ds.Batching
		var fm *filestore.FileManager
		packpath := filepath.Join(workdir, ".ipfs-pack")
		if fsrepo.IsInitialized(packpath) {
			r, err := getRepo(workdir)
			if err != nil {
				return err
			}
			dstore = r.Datastore()
			fm = r.FileManager()
		} else {
			dstore = ds.NewNullDatastore()
		}

		_, ds := buildDagserv(dstore, fm)

		issue, err := verifyPack(ds, workdir, fi)
		if err != nil {
			return err
		}

		if !issue {
			fmt.Println("Pack verified successfully!")
		} else {
			fmt.Println("Pack verify found some corruption.")
		}
		return nil
	},
}

func verifyPack(ds dag.DAGService, workdir string, manif io.Reader) (bool, error) {
	imp := DefaultImporterSettings.String()

	var issue bool
	scan := bufio.NewScanner(manif)
	for scan.Scan() {
		parts := strings.SplitN(scan.Text(), "\t", 3)
		hash := parts[0]
		fmtstr := parts[1]
		path := parts[2]

		if fmtstr != imp {
			fmt.Printf("error: unsupported importer settings in manifest file: %s\n", fmtstr)
			issue = true
			continue
		}

		params := &h.DagBuilderParams{
			Dagserv:   ds,
			NoCopy:    true,
			RawLeaves: true,
			Maxlinks:  h.DefaultLinksPerBlock,
		}

		ok, err := verifyItem(path, hash, workdir, params)
		if err != nil {
			return false, err
		}
		if !ok {
			issue = true
		}
	}
	return issue, nil
}

func verifyItem(path, hash, workdir string, params *h.DagBuilderParams) (bool, error) {
	st, err := os.Lstat(filepath.Join(workdir, path))
	switch {
	case os.IsNotExist(err):
		fmt.Printf("error: item in manifest, missing from pack: %s\n", path)
		return false, nil
	default:
		fmt.Printf("error: checking file %s: %s\n", path, err)
		return false, nil
	case err == nil:
		// continue
	}

	if st.IsDir() {
		return true, nil
	}

	nd, err := addItem(filepath.Join(workdir, path), st, params)
	if err != nil {
		return false, err
	}

	if nd.Cid().String() != hash {
		fmt.Printf("Checksum mismatch on %s. (%s)\n", path, nd.Cid().String())
		return false, nil
	}
	return true, nil
}

func addItem(path string, st os.FileInfo, params *h.DagBuilderParams) (node.Node, error) {
	if st.Mode()&os.ModeSymlink != 0 {
		trgt, err := os.Readlink(path)
		if err != nil {
			return nil, err
		}

		data, err := ft.SymlinkData(trgt)
		if err != nil {
			return nil, err
		}

		nd := new(dag.ProtoNode)
		nd.SetData(data)
		return nd, nil
	}

	fi, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer fi.Close()

	rf, err := files.NewReaderPathFile(filepath.Base(path), path, fi, st)
	if err != nil {
		return nil, err
	}

	spl := chunk.NewSizeSplitter(rf, chunk.DefaultBlockSize)
	dbh := params.New(spl)

	nd, err := balanced.BalancedLayout(dbh)
	if err != nil {
		return nil, err
	}

	return nd, nil
}
