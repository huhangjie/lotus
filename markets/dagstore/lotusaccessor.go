package dagstore

import (
	"context"
	"fmt"
	"io"
	"os"
	"strconv"

	"github.com/filecoin-project/dagstore/throttle"
	"github.com/ipfs/go-cid"
	"golang.org/x/xerrors"

	"github.com/filecoin-project/go-fil-markets/piecestore"
	"github.com/filecoin-project/go-fil-markets/retrievalmarket"
	"github.com/filecoin-project/go-fil-markets/shared"
)

// MaxConcurrentStorageCalls caps the amount of concurrent calls to the
// storage, so that we don't spam it during heavy processes like bulk migration.
var MaxConcurrentStorageCalls = func() int {
	// TODO replace env with config.toml attribute.
	v, ok := os.LookupEnv("LOTUS_DAGSTORE_MOUNT_CONCURRENCY")
	if ok {
		concurrency, err := strconv.Atoi(v)
		if err == nil {
			return concurrency
		}
	}
	return 100
}()

type LotusAccessor interface {
	FetchUnsealedPiece(ctx context.Context, pieceCid cid.Cid) (io.ReadCloser, error)
	GetUnpaddedCARSize(ctx context.Context, pieceCid cid.Cid) (uint64, error)
	IsUnsealed(ctx context.Context, pieceCid cid.Cid) (bool, error)
	Start(ctx context.Context) error
}

type lotusAccessor struct {
	pieceStore piecestore.PieceStore
	rm         retrievalmarket.RetrievalProviderNode
	throttle   throttle.Throttler
	readyMgr   *shared.ReadyManager
}

var _ LotusAccessor = (*lotusAccessor)(nil)

func NewLotusAccessor(store piecestore.PieceStore, rm retrievalmarket.RetrievalProviderNode) LotusAccessor {
	return &lotusAccessor{
		pieceStore: store,
		rm:         rm,
		throttle:   throttle.Fixed(MaxConcurrentStorageCalls),
		readyMgr:   shared.NewReadyManager(),
	}
}

func (m *lotusAccessor) Start(_ context.Context) error {
	return m.readyMgr.FireReady(nil)
}

func (m *lotusAccessor) IsUnsealed(ctx context.Context, pieceCid cid.Cid) (bool, error) {
	err := m.readyMgr.AwaitReady()
	if err != nil {
		return false, xerrors.Errorf("failed while waiting for accessor to start: %w", err)
	}


	var pieceInfo piecestore.PieceInfo
	err = m.throttle.Do(ctx, func(ctx context.Context) (err error) {
		pieceInfo, err = m.pieceStore.GetPieceInfo(pieceCid)
		return err
	})

	if err != nil {
		return false, xerrors.Errorf("failed to fetch pieceInfo for piece %s: %w", pieceCid, err)
	}

	if len(pieceInfo.Deals) == 0 {
		return false, xerrors.Errorf("no storage deals found for piece %s", pieceCid)
	}

	// check if we have an unsealed deal for the given piece in any of the unsealed sectors.
	for _, deal := range pieceInfo.Deals {
		deal := deal

		var isUnsealed bool
		// Throttle this path to avoid flooding the storage subsystem.
		err := m.throttle.Do(ctx, func(ctx context.Context) (err error) {
			isUnsealed, err = m.rm.IsUnsealed(ctx, deal.SectorID, deal.Offset.Unpadded(), deal.Length.Unpadded())
			if err != nil {
				return fmt.Errorf("failed to check if sector %d for deal %d was unsealed: %w", deal.SectorID, deal.DealID, err)
			}
			return nil
		})

		if err != nil {
			log.Warnf("failed to check/retrieve unsealed sector: %s", err)
			continue // move on to the next match.
		}

		if isUnsealed {
			return true, nil
		}
	}

	// we don't have an unsealed sector containing the piece
	return false, nil
}

func (m *lotusAccessor) FetchUnsealedPiece(ctx context.Context, pieceCid cid.Cid) (io.ReadCloser, error) {
	err := m.readyMgr.AwaitReady()
	if err != nil {
		return nil, err
	}

	// Throttle this path to avoid flooding the storage subsystem.
	var pieceInfo piecestore.PieceInfo
	err = m.throttle.Do(ctx, func(ctx context.Context) (err error) {
		pieceInfo, err = m.pieceStore.GetPieceInfo(pieceCid)
		return err
	})

	if err != nil {
		return nil, xerrors.Errorf("failed to fetch pieceInfo for piece %s: %w", pieceCid, err)
	}

	if len(pieceInfo.Deals) == 0 {
		return nil, xerrors.Errorf("no storage deals found for piece %s", pieceCid)
	}

	// prefer an unsealed sector containing the piece if one exists
	for _, deal := range pieceInfo.Deals {
		deal := deal

		// Throttle this path to avoid flooding the storage subsystem.
		var reader io.ReadCloser
		err := m.throttle.Do(ctx, func(ctx context.Context) (err error) {
			isUnsealed, err := m.rm.IsUnsealed(ctx, deal.SectorID, deal.Offset.Unpadded(), deal.Length.Unpadded())
			if err != nil {
				return fmt.Errorf("failed to check if sector %d for deal %d was unsealed: %w", deal.SectorID, deal.DealID, err)
			}
			if !isUnsealed {
				return nil
			}
			// Because we know we have an unsealed copy, this UnsealSector call will actually not perform any unsealing.
			reader, err = m.rm.UnsealSector(ctx, deal.SectorID, deal.Offset.Unpadded(), deal.Length.Unpadded())
			return err
		})

		if err != nil {
			log.Warnf("failed to check/retrieve unsealed sector: %s", err)
			continue // move on to the next match.
		}

		if reader != nil {
			// we were able to obtain a reader for an already unsealed piece
			return reader, nil
		}
	}

	lastErr := xerrors.New("no sectors found to unseal from")
	// if there is no unsealed sector containing the piece, just read the piece from the first sector we are able to unseal.
	for _, deal := range pieceInfo.Deals {
		// Note that if the deal data is not already unsealed, unsealing may
		// block for a long time with the current PoRep
		//
		// This path is unthrottled.
		reader, err := m.rm.UnsealSector(ctx, deal.SectorID, deal.Offset.Unpadded(), deal.Length.Unpadded())
		if err != nil {
			lastErr = xerrors.Errorf("failed to unseal deal %d: %w", deal.DealID, err)
			log.Warn(lastErr.Error())
			continue
		}

		// Successfully fetched the deal data so return a reader over the data
		return reader, nil
	}

	return nil, lastErr
}

func (m *lotusAccessor) GetUnpaddedCARSize(ctx context.Context, pieceCid cid.Cid) (uint64, error) {
	err := m.readyMgr.AwaitReady()
	if err != nil {
		return 0, err
	}

	pieceInfo, err := m.pieceStore.GetPieceInfo(pieceCid)
	if err != nil {
		return 0, xerrors.Errorf("failed to fetch pieceInfo for piece %s: %w", pieceCid, err)
	}

	if len(pieceInfo.Deals) == 0 {
		return 0, xerrors.Errorf("no storage deals found for piece %s", pieceCid)
	}

	len := pieceInfo.Deals[0].Length

	return uint64(len), nil
}
