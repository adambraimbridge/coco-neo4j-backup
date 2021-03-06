package main

import (
	"os"
	"time"
	"io"
	log "github.com/Sirupsen/logrus"
	"github.com/urfave/cli"
	"archive/tar"
	"fmt"
)

const archiveNameDateFormat = "2006-01-02T15-04-05"

var tarWriter *tar.Writer

type config struct {
	fleetEndpoint string
	socksProxy string
	awsAccessKey string
	awsSecretKey string
	dataFolder string
	targetFolder string
	s3Domain string
	bucketName string
	env string
}

func main() {
	startTime := time.Now()
	log.Infof("Starting backup operation at startTime=%d.", startTime)

	app := cli.NewApp()
	app.Name = "Universal Publishing CoCo neo4j Backup Service"
	app.Usage = "Execute a cold backup of a neo4j instance inside a CoCo cluster and upload it to AWS S3."
	app.Flags = []cli.Flag {
		cli.StringFlag{
			Name: "fleetEndpoint",
			Value: "http://localhost:49153",
			Usage: "connect to fleet API at `URL`",
			EnvVar: "FLEETCTL_ENDPOINT",
		},
		cli.StringFlag{
			Name: "socksProxy",
			Value: "",
			Usage: "connect to fleet via SOCKS proxy at `PROXY` in IP:PORT format",
			EnvVar: "SOCKS_PROXY",
		},
		cli.StringFlag{
			Name: "awsAccessKey",
			Value: "",
			Usage: "connect to AWS API using access key `KEY`",
			EnvVar: "AWS_ACCESS_KEY",
		},
		cli.StringFlag{
			Name: "awsSecretKey",
			Value: "",
			Usage: "connect to AWS API using secret key `KEY`",
			EnvVar: "AWS_SECRET_KEY",
		},
		cli.StringFlag{
			Name: "dataFolder",
			Value: "/data/graph.db/",
			Usage: "back up from data folder `DATA_FOLDER` (needs a trailing slash)",
			EnvVar: "DATA_FOLDER",
		},
		cli.StringFlag{
			Name: "targetFolder",
			Value: "/data/graph.db.backup",
			Usage: "back up to data folder `TARGET_FOLDER`",
			EnvVar: "TARGET_FOLDER",
		},
		cli.StringFlag{
			Name: "s3Domain",
			Value: "s3-eu-west-1.amazonaws.com",
			Usage: "upload archive to S3 with domain (i.e. hostname) `S3_DOMAIN`",
			EnvVar: "S3_DOMAIN",
		},
		cli.StringFlag{
			Name: "bucketName",
			Value: "com.ft.universalpublishing.backup-data",
			Usage: "upload archive to S3 with bucket name `BUCKET_NAME`",
			EnvVar: "BUCKET_NAME",
		},
		cli.StringFlag{
			Name: "env",
			Value: "",
			Usage: "connect to CoCo environment with tag `ENVIRONMENT_TAG`",
			EnvVar: "ENVIRONMENT_TAG",
		},
	}
	app.Action = func(c *cli.Context) error {
		err := runOuter(config{
			c.String("fleetEndpoint"), // fleet
			c.String("socksProxy"),    // fleet
			c.String("awsAccessKey"),  // S3
			c.String("awsSecretKey"),  // S3
			c.String("dataFolder"),    // filesystem
			c.String("targetFolder"),  // filesystem
			c.String("s3Domain"),      // S3
			c.String("bucketName"),    // S3
			c.String("env"),           // filesystem
		})
		if err != nil {
			os.Exit(1)
		}
		log.WithFields(log.Fields{"duration": time.Since(startTime).String()}).Info("Backup process complete.")
		return err
	}

	app.Run(os.Args)
}

func runOuter(cfg config) (error) {

	fleetClient, err := newFleetClient(cfg.fleetEndpoint, cfg.socksProxy)
	if err != nil {
		log.WithFields(log.Fields{
			"fleetEndpoint": cfg.fleetEndpoint,
			"socksProxy": cfg.socksProxy,
			"err": err,
		}).Error("Error instantiating fleet client; backup process failed.")
		return err
	}
	archiveName := fmt.Sprintf("neo4j_backup_%s_%s.tar.gz", time.Now().UTC().Format(archiveNameDateFormat), cfg.env)

	bucketWriter, err := newBucketWriter(cfg.awsAccessKey, cfg.awsSecretKey, cfg.s3Domain, cfg.bucketName, archiveName)
	if err != nil {
		log.WithFields(log.Fields{"err": err}).Error("Error instantiating S3 bucket writer; backup process failed.")
		return err
	}

	return runInner(fleetClient, bucketWriter, cfg.dataFolder, cfg.targetFolder, archiveName)
}

func runInner(
	fleetClient fleetAPI,
	bucketWriter io.WriteCloser,
	dataFolder string,
	targetFolder string,
	archiveName string,
	) (error) {

	log.WithFields(log.Fields{
		"dataFolder": dataFolder,
		"targetFolder": targetFolder,
	}).Info("Starting first hot rsync process.")
	err := rsync(dataFolder, targetFolder)
	if err != nil {
		log.WithFields(log.Fields{
			"dataFolder": dataFolder,
			"targetFolder": targetFolder,
			"err": err,
		}).Warn("Error synchronising neo4j files while database is running (i.e. hot); re-trying once.")
		err = rsync(dataFolder, targetFolder)
		if err != nil {
			log.WithFields(log.Fields{
				"dataFolder": dataFolder,
				"targetFolder": targetFolder,
				"err": err,
			}).Warn("Encountered another error synchronising neo4j files while database is running " +
				"(i.e. hot); backup process failed to complete successfully. " +
				"The cold backup phase will consequently take longer than usual.")
		}
	}
	log.Info("Hot rsync completed, shutting down neo...")
	err = shutDownNeo(fleetClient)
	if err != nil {
		log.WithFields(log.Fields{"err": err}).Error("Error shutting down neo4j; backup process failed.")
		return err
	}
	log.WithFields(log.Fields{
		"dataFolder": dataFolder,
		"targetFolder": targetFolder,
	}).Info("Starting cold rsync process...")
	err = rsync(dataFolder, targetFolder)
	if err != nil {
		log.WithFields(log.Fields{
			"dataFolder": dataFolder,
			"targetFolder": targetFolder,
			"err": err,
		}).Error("Error synchronising neo4j files while database is stopped; backup process failed.")
		return err
	}
	log.Info("cold rsync completed, restarting neo...")
	err = startNeo(fleetClient)
	if err != nil {
		log.WithFields(log.Fields{"err": err}).Error("Error starting up neo4j.")
		return err
	}
	log.Info("neo has been started up, commencing archive creation...")
	pipeReader, err := createBackup(targetFolder, archiveName)
	if err != nil {
		log.WithFields(log.Fields{"err": err}).Error("Error creating backup tarball.")
		return err
	}
	log.WithFields(log.Fields{"archiveName": archiveName, "err": err}).Info("Initial tar/gzip archive created, streaming data to S3 as it is added to the archive...")
	err = uploadToS3(bucketWriter, pipeReader)
	if err != nil {
		log.WithFields(log.Fields{"archiveName": archiveName, "err": err}).Error("Error uploading to S3; backup process failed.")
		return err
	}
	validateEnvironment()
	log.WithFields(log.Fields{"archiveName": archiveName}).Info("Artefact successfully uploaded to S3; backup process complete.")
	return nil
}

func newBucketWriter(awsAccessKey string, awsSecretKey string, s3Domain string, bucketName string, archiveName string) (io.WriteCloser, error) {
	bucketWriterProvider := newS3WriterProvider(awsAccessKey, awsSecretKey, s3Domain, bucketName)
	bucketWriter, err := bucketWriterProvider.getWriter(archiveName)
	if err != nil {
		log.Error("BucketWriter cannot be created: "+err.Error(), err)
	}
	return bucketWriter, err
}

func uploadToS3(bucketWriter io.WriteCloser, pipeReader *io.PipeReader) (err error) {
	defer bucketWriter.Close()

	//upload the archive to the bucket
	_, err = io.Copy(bucketWriter, pipeReader)
	if err != nil {
		log.Error("Cannot upload archive to S3: "+err.Error(), err)
		return err
	}
	pipeReader.Close()
	return nil
}
