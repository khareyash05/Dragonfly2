/*
 *     Copyright 2022 The Dragonfly Authors
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *      http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

//go:generate mockgen -destination mocks/announcer_mock.go -source announcer.go -package mocks

package announcer

import (
	"context"
	"fmt"
	"io"
	"time"

	"golang.org/x/sync/errgroup"

	managerv2 "d7y.io/api/pkg/apis/manager/v2"
	trainerv1 "d7y.io/api/pkg/apis/trainer/v1"

	logger "d7y.io/dragonfly/v2/internal/dflog"
	managerclient "d7y.io/dragonfly/v2/pkg/rpc/manager/client"
	trainerclient "d7y.io/dragonfly/v2/pkg/rpc/trainer/client"
	"d7y.io/dragonfly/v2/scheduler/config"
	"d7y.io/dragonfly/v2/scheduler/storage"
)

const (
	// UploadBufferSize is the buffer size for upload.
	UploadBufferSize = 1024 * 1024
)

// Announcer is the interface used for announce service.
type Announcer interface {
	// Started announcer server.
	Serve() error

	// Stop announcer server.
	Stop() error
}

// announcer provides announce function.
type announcer struct {
	config        *config.Config
	managerClient managerclient.V2
	trainerClient trainerclient.V1
	storage       storage.Storage
	done          chan struct{}
}

// WithTrainerClient sets the grpc client of trainer.
func WithTrainerClient(client trainerclient.V1) Option {
	return func(a *announcer) {
		a.trainerClient = client
	}
}

// Option is a functional option for configuring the announcer.
type Option func(s *announcer)

// New returns a new Announcer interface.
func New(cfg *config.Config, managerClient managerclient.V2, storage storage.Storage, options ...Option) (Announcer, error) {
	a := &announcer{
		config:        cfg,
		managerClient: managerClient,
		storage:       storage,
		done:          make(chan struct{}),
	}

	for _, opt := range options {
		opt(a)
	}

	// Register to manager.
	if _, err := a.managerClient.UpdateScheduler(context.Background(), &managerv2.UpdateSchedulerRequest{
		SourceType:         managerv2.SourceType_SCHEDULER_SOURCE,
		Hostname:           a.config.Server.Host,
		Ip:                 a.config.Server.AdvertiseIP.String(),
		Port:               int32(a.config.Server.AdvertisePort),
		Idc:                a.config.Host.IDC,
		Location:           a.config.Host.Location,
		SchedulerClusterId: uint64(a.config.Manager.SchedulerClusterID),
	}); err != nil {
		return nil, err
	}

	return a, nil
}

// Started announcer server.
func (a *announcer) Serve() error {
	logger.Info("announce scheduler to manager")
	if err := a.announceToManager(); err != nil {
		return err
	}

	if a.trainerClient != nil {
		logger.Info("announce scheduler to trainer")
		if err := a.announceToTrainer(); err != nil {
			return err
		}
	}

	return nil
}

// Stop announcer server.
func (a *announcer) Stop() error {
	close(a.done)
	return nil
}

// announceSeedPeer announces peer information to manager.
func (a *announcer) announceToManager() error {
	// Start keepalive to manager.
	go func() {
		a.managerClient.KeepAlive(a.config.Manager.KeepAlive.Interval, &managerv2.KeepAliveRequest{
			SourceType: managerv2.SourceType_SCHEDULER_SOURCE,
			Hostname:   a.config.Server.Host,
			Ip:         a.config.Server.AdvertiseIP.String(),
			ClusterId:  uint64(a.config.Manager.SchedulerClusterID),
		}, a.done)
	}()

	return nil
}

// announceSeedPeer announces dataset to trainer.
func (a *announcer) announceToTrainer() error {
	tick := time.NewTicker(a.config.Trainer.Interval)
	for {
		select {
		case <-tick.C:
			if err := a.train(); err != nil {
				logger.Error(err)
			}
		case <-a.done:
			return nil
		}
	}
}

// train uploads dataset to trainer and trigger training.
func (a *announcer) train() error {
	ctx, cancel := context.WithTimeout(context.Background(), a.config.Trainer.UploadTimeout)
	defer cancel()

	stream, err := a.trainerClient.Train(ctx)
	if err != nil {
		return err
	}

	eg := errgroup.Group{}
	eg.Go(func() error {
		if err := a.uploadDownloadToTrainer(stream); err != nil {
			return fmt.Errorf("upload download: %w", err)
		}

		return nil
	})

	eg.Go(func() error {
		if err := a.uploadNetworkTopologyToTrainer(stream); err != nil {
			return fmt.Errorf("upload network topology: %w", err)
		}

		return nil
	})

	if err := eg.Wait(); err != nil {
		return err
	}

	if _, err := stream.CloseAndRecv(); err != nil {
		return err
	}

	return nil
}

// uploadDownloadToTrainer uploads download information to trainer.
func (a *announcer) uploadDownloadToTrainer(stream trainerv1.Trainer_TrainClient) error {
	readCloser, err := a.storage.OpenDownload()
	if err != nil {
		return err
	}
	defer readCloser.Close()

	buf := make([]byte, UploadBufferSize)
	for {
		n, err := readCloser.Read(buf)
		if err != nil && err != io.EOF {
			return err
		}

		if err := stream.Send(&trainerv1.TrainRequest{
			Hostname:  a.config.Server.Host,
			Ip:        a.config.Server.AdvertiseIP.String(),
			ClusterId: uint64(a.config.Manager.SchedulerClusterID),
			Request: &trainerv1.TrainRequest_TrainMlpRequest{
				TrainMlpRequest: &trainerv1.TrainMLPRequest{
					Dataset: buf[:n],
				},
			},
		}); err != nil {
			return err
		}

		if err == io.EOF {
			break
		}
	}

	return nil
}

// uploadNetworkTopologyToTrainer uploads network topology to trainer.
func (a *announcer) uploadNetworkTopologyToTrainer(stream trainerv1.Trainer_TrainClient) error {
	readCloser, err := a.storage.OpenNetworkTopology()
	if err != nil {
		return err
	}
	defer readCloser.Close()

	buf := make([]byte, UploadBufferSize)
	for {
		n, err := readCloser.Read(buf)
		if err != nil && err != io.EOF {
			return err
		}

		if err := stream.Send(&trainerv1.TrainRequest{
			Hostname:  a.config.Server.Host,
			Ip:        a.config.Server.AdvertiseIP.String(),
			ClusterId: uint64(a.config.Manager.SchedulerClusterID),
			Request: &trainerv1.TrainRequest_TrainGnnRequest{
				TrainGnnRequest: &trainerv1.TrainGNNRequest{
					Dataset: buf[:n],
				},
			},
		}); err != nil {
			return err
		}

		if err == io.EOF {
			break
		}
	}

	return nil
}
