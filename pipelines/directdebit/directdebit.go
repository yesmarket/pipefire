package directdebit

import (
	"encoding/json"
	"errors"
	"fmt"
	"runtime"
	"strings"

	mysql "github.com/go-sql-driver/mysql"
	"github.com/jinzhu/gorm"
	log "github.com/sirupsen/logrus"
)

// Pipeline is an implementation of a pipeline
type Pipeline interface {
	StartListener(chan error)
	Execute(string) []error
	Close() error
	sftpGet(conf *SftpConfig) error
	sftpTo(conf *SftpConfig) error
	archiveTransferred(conf *ArchiveConfig) error
	cleanDirtyFiles(conf *CleanUpConfig) []error
	pgpEncryptFilesForBank(conf *EncryptFilesConfig) []error
}

//TasksConfig Configuration
type TasksConfig struct {
	GetFilesFromBFP    SftpConfig         `json:"getFilesFromBFP"`
	CleanBFP           SftpConfig         `json:"cleanBFP"`
	EncryptFiles       EncryptFilesConfig `json:"encrypteFiles"`
	SftpFilesToANZ     SftpConfig         `json:"sftpFilesToANZ"`
	SftpFilesToPx      SftpConfig         `json:"sftpFilesToPx"`
	SftpFilesToBNZ     SftpConfig         `json:"sftpFilesToBNZ"`
	ArchiveTransferred ArchiveConfig      `json:"archiveTransferred"`
	CleanDirtyFiles    CleanUpConfig      `json:"cleanDirtyFiles"`
}

// PipelineConfig defines the required arguements for the pipeline
type PipelineConfig struct {
	Database mysql.Config `json:"database"`
	Rabbitmq BusConfig    `json:"rabbitmq"`
	Tasks    TasksConfig  `json:"tasks"`
}

type ddPipeline struct {
	log           *log.Entry
	correlationID string
	consumer      *MessageConsumer
	transferlog   *TransferLog
	taskConfig    *PipelineConfig
}

// New Pipeline
func New(config *PipelineConfig) (Pipeline, error) {

	var p *ddPipeline = &ddPipeline{
		taskConfig: config,
		log:        log.WithField("Pipeline", "DirectDebit"),
	}

	if false && config.Database.Addr != "" {
		dbConfig := config.Database
		dbConfig.ParseTime = true

		redact := func(r rune) rune {
			return '*'
		}

		redactedPw := strings.Map(redact, dbConfig.Passwd)

		log.Debugf("Connection String (pw redacted): %s:%s@/%s", dbConfig.User, redactedPw, dbConfig.Addr)

		if err := mysql.SetLogger(p.log); err != nil {
			return nil, err
		}

		// if config.Database {
		connectionString := dbConfig.FormatDSN()
		db, err := gorm.Open("mysql", connectionString)
		if err != nil {
			return nil, fmt.Errorf("Unable to connect to the database: %s", err.Error())
		}
		db.SetLogger(p.log)
		db.LogMode(true)

		p.transferlog = NewRecorder(db, p.log)
	}

	if config.Rabbitmq.Host != "" {
		consumer, err := NewConsumer(&config.Rabbitmq, p.log)
		if err != nil {
			return nil, err
		}
		p.consumer = &consumer
	}

	return p, nil
}

func (p *ddPipeline) StartListener(errCh chan error) {

	consumer := *p.consumer

	if err := consumer.Configure(); err != nil {
		p.log.Errorf("Unable to register Exchanges and Queues", err.Error())
	}

	deliveryChannel, err := consumer.Consume()
	if err != nil {
		p.log.Error("Unable to Consume Messages %s", err.Error())

		// Send the error to the channel
		errCh <- err
	}

	log.Info("Consumer Registered Listening")

	for d := range deliveryChannel {
		log.Tracef("Message Body: [x] %s", d.Body)
		log.Debugf("#goroutines: %d\n", runtime.NumGoroutine())
		// json decode
		payload := &TransferFilesPayload{}
		if err := json.Unmarshal(d.Body, payload); err != nil {
			// @todo publish message to the error queue
			log.Error("Unable to unmarshal the message. Please ensure that the message is valid")
			log.Error("Invalid Message is %s ", d.Body)
		}

		correlationID := payload.Message.CorrelationID

		if len(correlationID) > 0 {
			errs := p.Execute(correlationID)
			if len(errs) > 0 {
				log.Warning("Pipeline Completed With Errors")
				for _, e := range errs {
					log.Error(e.Error())
				}
			} else {
				log.Info("Pipeline Completed Without Errors")
			}
		} else {
			log.Error("Message payload did not include a correlationId, aborting. Resend Message with a correlationId")
		}

	}

	log.Warning("Listener Stopped")
	errCh <- errors.New("Channel went away")
}

// Execute starts the execution of the pipeline
func (p *ddPipeline) Execute(correlationID string) (errorList []error) {

	p.correlationID = correlationID
	p.log = log.WithField("correlationId", correlationID)

	// @todo put this into a workflow
	log.Info("Starting Direct Debit Pipeline")

	// @todo config validation
	// @todo turn into loop
	if err := p.getFilesFromBFP(); err != nil {
		// we need the files from the BFP otherwise there is no point
		return append(errorList, err)
	}

	if err := p.cleanBFP(); err != nil {
		// not a big deal if cleaning fails..we can clean it up after
		errorList = append(errorList, err)
	}

	if err := p.encryptFiles(); err != nil {
		// We need all the files encrypted
		// before we continue further
		return err
	}

	// Transfer the files
	if err := p.sftpFilesToANZ(); err != nil {
		errorList = append(errorList, err)
	}

	if err := p.sftpFilesToPx(); err != nil {
		errorList = append(errorList, err)
	}

	if err := p.sftpFilesToBNZ(); err != nil {
		errorList = append(errorList, err)
	}

	// Archive the folder
	if err := p.archive(); err != nil {
		errorList = append(errorList, err)
	}

	// remove all the plain text files
	if err := p.cleanUp(); err != nil {
		errorList = append(errorList, err...)
	}

	if len(errorList) > 0 {
		log.Error("END DD Pipeline with Errors")
	} else {
		log.Info("END DD Pipeline Without Errors")
	}

	return errorList
}

func (p *ddPipeline) Close() error {
	p.log.Info("Recieved Shutdown Request")
	if p.transferlog != nil && p.transferlog.Conn != nil {
		p.log.Info("Shutdown Database Connection")
		if err := p.transferlog.Conn.Close(); err != nil {
			p.log.Warningf("Error closing database connecton, %s", err.Error())
		}
		p.log.Info("Shutdown Database Complete")
	}

	if p.consumer != nil {
		p.log.Info("Shutdown RabbitMQ Connection")
		consumer := *p.consumer
		if err := consumer.Close(); err != nil {
			p.log.Warningf("Error closing RabbitMQ connecton, %s", err.Error())
			return err
		}
		p.log.Info("Shutdown RabbitMQ Complete")
	}

	p.log.Info("Shutdown Complete")
	return nil
}

func (p *ddPipeline) archive() error {
	p.log.Info("Archiving Transferred Files")

	archiveConfig := p.taskConfig.Tasks.ArchiveTransferred
	if archiveConfig.Enabled {
		if err := p.archiveTransferred(&archiveConfig); err != nil {
			p.log.Error(err.Error())
			return err
		}
		p.log.Info("Archiving Transferred Files Complete")
	} else {
		p.log.Warn("Archiving Transferred Files Skipped")
	}

	return nil
}

func (p *ddPipeline) cleanUp() (err []error) {
	p.log.Info("Clean Up Start")
	cleanUpConfig := p.taskConfig.Tasks.CleanDirtyFiles
	if cleanUpConfig.Enabled {
		err = p.cleanDirtyFiles(&cleanUpConfig)
		p.log.Info("Clean Up Complete")
	} else {
		p.log.Warn("Clean Up Files Skipped")
	}

	return err
}

func (p *ddPipeline) getFilesFromBFP() error {

	p.log.Info("GetFilesFromBFP Start")
	bfpSftp := p.taskConfig.Tasks.GetFilesFromBFP
	if bfpSftp.Enabled {
		if err := p.sftpGet(&bfpSftp); err != nil {
			p.log.Error("Error Collecting the files. Unable to continue without files..Aborting")
			return err
		}
		p.log.Info("GetFilesFromBFP Complete")
		return nil
	}
	p.log.Warn("GetFilesFromBFP Skipped")

	return nil
}

func (p *ddPipeline) cleanBFP() error {

	p.log.Info("CleanBFP Start")
	bfpClean := p.taskConfig.Tasks.CleanBFP
	if bfpClean.Enabled {
		if err := p.sftpClean(&bfpClean); err != nil {
			p.log.Warningf("Unable to clean remote dir %s", err.Error())
			return err
		}
		return nil
	}
	p.log.Warn("CleanBFP Skipped")
	return nil
}

func (p *ddPipeline) encryptFiles() []error {
	p.log.Info("EncryptFiles Start")
	encryptionConfig := p.taskConfig.Tasks.EncryptFiles
	if encryptionConfig.Enabled {
		if err := p.pgpEncryptFilesForBank(&encryptionConfig); err != nil {
			p.log.Error("Unable to encrypt all files..Aborting")
			return err
		}
		p.log.Info("Encrypt Files Complete")
		return nil
	}
	p.log.Warn("Encrypt Files Skipped")
	return nil
}

func (p *ddPipeline) sftpFilesToANZ() error {

	p.log.Info("SftpFilesToANZ Start")

	anzSftp := p.taskConfig.Tasks.SftpFilesToANZ
	if anzSftp.Enabled {
		if err := p.sftpTo(&anzSftp); err != nil {
			return err
		}
		p.log.Info("SftpFilesToANZ Complete")
		return nil
	}
	p.log.Warn("SftpFilesToANZ Skipped")

	return nil
}

func (p *ddPipeline) sftpFilesToPx() error {
	p.log.Info("SftpFilesToPx Start")
	pxSftp := p.taskConfig.Tasks.SftpFilesToPx
	if pxSftp.Enabled {
		if err := p.sftpTo(&pxSftp); err != nil {
			return err
		}
		p.log.Info("SftpFilesToPx Complete")
		return nil
	}
	p.log.Warn("SftpFilesToPx Skipped")

	return nil
}

func (p *ddPipeline) sftpFilesToBNZ() error {
	p.log.Info("SftpFilesToBNZ Start")

	bnzSftp := p.taskConfig.Tasks.SftpFilesToBNZ
	if bnzSftp.Enabled {
		if err := p.sftpTo(&bnzSftp); err != nil {
			return err
		}
		p.log.Info("SftpFilesToBNZ Complete")
		return nil
	}

	p.log.Warn("SftpFilesToBNZ Skipped")
	return nil
}
