// Copyright (C) MongoDB, Inc. 2017-present.
//
// Licensed under the Apache License, Version 2.0 (the "License"); you may
// not use this file except in compliance with the License. You may obtain
// a copy of the License at http://www.apache.org/licenses/LICENSE-2.0

package integration

import (
	"testing"
	"time"

	"github.com/appveen/mongo-go-driver/bson"
	"github.com/appveen/mongo-go-driver/bson/primitive"
	"github.com/appveen/mongo-go-driver/internal/testutil/assert"
	"github.com/appveen/mongo-go-driver/mongo"
	"github.com/appveen/mongo-go-driver/mongo/integration/mtest"
	"github.com/appveen/mongo-go-driver/mongo/options"
)

type resumeType int
type streamType int

const (
	minChangeStreamVersion = "3.6.0"
	minPbrtVersion         = "4.0.7"
	minStartAfterVersion   = "4.1.1"

	startAfter resumeType = iota
	resumeAfter
	operationTime

	client streamType = iota
	database
	collection

	errorInterrupted        int32 = 11601
	errorCappedPositionLost int32 = 136
	errorCursorKilled       int32 = 237
)

func TestChangeStream_Standalone(t *testing.T) {
	mtOpts := mtest.NewOptions().MinServerVersion(minChangeStreamVersion).CreateClient(false).Topologies(mtest.Single)
	mt := mtest.New(t, mtOpts)
	defer mt.Close()

	mt.Run("no custom standalone error", func(mt *mtest.T) {
		_, err := mt.Coll.Watch(mtest.Background, mongo.Pipeline{})
		_, ok := err.(mongo.CommandError)
		assert.True(mt, ok, "expected error type %T, got %T", mongo.CommandError{}, err)
	})
}

func TestChangeStream_ReplicaSet(t *testing.T) {
	mtOpts := mtest.NewOptions().MinServerVersion(minChangeStreamVersion).CreateClient(false).Topologies(mtest.ReplicaSet)
	mt := mtest.New(t, mtOpts)
	defer mt.Close()

	mt.Run("first stage is $changeStream", func(mt *mtest.T) {
		// first stage in the aggregate pipeline must be $changeStream

		cs, err := mt.Coll.Watch(mtest.Background, mongo.Pipeline{})
		assert.Nil(mt, err, "Watch error: %v", err)
		defer closeStream(cs)
		started := mt.GetStartedEvent()
		assert.NotNil(mt, started, "expected started event for aggregate, got nil")

		// pipeline is array of documents. first value of first element in array is the first stage document
		firstStage := started.Command.Lookup("pipeline").Array().Index(0).Value().Document()
		elems, _ := firstStage.Elements()
		assert.Equal(mt, 1, len(elems), "expected first stage document to have 1 element, got %v", len(elems))
		firstKey := elems[0].Key()
		want := "$changeStream"
		assert.Equal(mt, want, firstKey, "expected first stage to be %v, got %v", want, firstKey)
	})
	mt.Run("track resume token", func(mt *mtest.T) {
		// ChangeStream must continuously track the last seen resumeToken

		cs, err := mt.Coll.Watch(mtest.Background, mongo.Pipeline{})
		assert.Nil(mt, err, "Watch error: %v", err)
		defer closeStream(cs)

		generateEvents(mt, 1)
		assert.True(mt, cs.Next(mtest.Background), "expected next to return true, got false")
		assert.NotNil(mt, cs.ResumeToken(), "expected resume token, got nil")
	})
	mt.Run("missing resume token", func(mt *mtest.T) {
		// ChangeStream will throw an exception if the server response is missing the resume token

		projectDoc := bson.D{
			{"$project", bson.D{
				{"_id", 0},
			}},
		}
		cs, err := mt.Coll.Watch(mtest.Background, mongo.Pipeline{projectDoc})
		assert.Nil(mt, err, "Watch error: %v", err)
		defer closeStream(cs)

		generateEvents(mt, 2)
		assert.False(mt, cs.Next(mtest.Background), "expected Next to return false, got true")
		assert.NotNil(mt, cs.Err(), "expected error, got nil")
	})
	mt.Run("resume once", func(mt *mtest.T) {
		// ChangeStream will automatically resume one time on a resumable error

		cs, err := mt.Coll.Watch(mtest.Background, mongo.Pipeline{})
		assert.Nil(mt, err, "Watch error: %v", err)
		defer closeStream(cs)

		ensureResumeToken(mt, cs)
		// kill cursor to force resumable error
		killChangeStreamCursor(mt, cs)
		generateEvents(mt, 1)

		mt.ClearEvents()
		// change stream should resume once and get new change
		assert.True(mt, cs.Next(mtest.Background), "expected Next to return true, got false")
		// Next should cause getMore, killCursors, and aggregate to run
		assert.NotNil(mt, mt.GetStartedEvent(), "expected getMore event, got nil")
		assert.NotNil(mt, mt.GetStartedEvent(), "expected killCursors event, got nil")
		aggEvent := mt.GetStartedEvent()
		assert.NotNil(mt, aggEvent, "expected aggregate event, got nil")
		assert.Equal(mt, "aggregate", aggEvent.CommandName, "expected command name 'aggregate', got '%v'", aggEvent.CommandName)
	})
	mt.RunOpts("no resume for aggregate errors", mtest.NewOptions().ClientType(mtest.Mock), func(mt *mtest.T) {
		// ChangeStream will not attempt to resume on any error encountered while executing an aggregate command

		// aggregate response: empty batch but valid cursor ID
		// getMore response: resumable error
		// killCursors response: success
		// resumed aggregate response: error
		ns := mt.Coll.Database().Name() + "." + mt.Coll.Name()
		aggRes := mtest.CreateCursorResponse(1, ns, mtest.FirstBatch)
		getMoreRes := mtest.CreateCommandErrorResponse(mtest.CommandError{
			Code:    errorInterrupted + 1,
			Name:    "foo",
			Message: "bar",
		})
		killCursorsRes := mtest.CreateSuccessResponse()
		resumedAggRes := mtest.CreateCommandErrorResponse(mtest.CommandError{
			Code:    1,
			Name:    "foo",
			Message: "bar",
		})
		mt.AddMockResponses(aggRes, getMoreRes, killCursorsRes, resumedAggRes)

		cs, err := mt.Coll.Watch(mtest.Background, mongo.Pipeline{})
		assert.Nil(mt, err, "Watch error: %v", err)
		defer closeStream(cs)

		assert.False(mt, cs.Next(mtest.Background), "expected Next to return false, got true")
	})
	mt.RunOpts("no resume for non-resumable errors", mtest.NewOptions().CreateClient(false), func(mt *mtest.T) {
		// ChangeStream will not attempt to resume after encountering error code 11601 (Interrupted),
		// 136 (CappedPositionLost), or 237 (CursorKilled) while executing a getMore command.

		var testCases = []struct {
			name    string
			errCode int32
		}{
			{"interrupted", errorInterrupted},
			{"capped position lost", errorCappedPositionLost},
			{"cursor killed", errorCursorKilled},
		}

		mockOpts := mtest.NewOptions().ClientType(mtest.Mock)
		for _, tc := range testCases {
			mt.RunOpts(tc.name, mockOpts, func(mt *mtest.T) {
				// aggregate response: empty batch but valid cursor ID
				// getMore response: error
				ns := mt.Coll.Database().Name() + "." + mt.Coll.Name()
				aggRes := mtest.CreateCursorResponse(1, ns, mtest.FirstBatch)
				getMoreRes := mtest.CreateCommandErrorResponse(mtest.CommandError{
					Code:    tc.errCode,
					Name:    "foo",
					Message: "bar",
				})
				mt.AddMockResponses(aggRes, getMoreRes)

				cs, err := mt.Coll.Watch(mtest.Background, mongo.Pipeline{})
				assert.Nil(mt, err, "Watch error: %v", err)
				defer closeStream(cs)

				assert.False(mt, cs.Next(mtest.Background), "expected Next to return false, got true")
				err = cs.Err()
				assert.NotNil(mt, err, "expected change stream error, got nil")
				cmdErr, ok := err.(mongo.CommandError)
				assert.True(mt, ok, "expected error type %v, got %v", mongo.CommandError{}, err)
				assert.Equal(mt, tc.errCode, cmdErr.Code, "expected code %v, got %v", tc.errCode, cmdErr.Code)
			})
		}
	})
	mt.RunOpts("server selection before resume", mtest.NewOptions().CreateClient(false), func(mt *mtest.T) {
		// ChangeStream will perform server selection before attempting to resume, using initial readPreference
		mt.Skip("skipping for lack of SDAM monitoring")
	})
	mt.Run("empty batch cursor not closed", func(mt *mtest.T) {
		// Ensure that a cursor returned from an aggregate command with a cursor id and an initial empty batch is not closed

		cs, err := mt.Coll.Watch(mtest.Background, mongo.Pipeline{})
		assert.Nil(mt, err, "Watch error: %v", err)
		defer closeStream(cs)
		assert.True(mt, cs.ID() > 0, "expected non-zero ID, got 0")
	})
	mt.RunOpts("ignore errors from killCursors", mtest.NewOptions().ClientType(mtest.Mock), func(mt *mtest.T) {
		// The killCursors command sent during the "Resume Process" must not be allowed to throw an exception.

		ns := mt.Coll.Database().Name() + "." + mt.Coll.Name()
		aggRes := mtest.CreateCursorResponse(1, ns, mtest.FirstBatch)
		getMoreRes := mtest.CreateCommandErrorResponse(mtest.CommandError{
			Code:    errorInterrupted + 1,
			Name:    "foo",
			Message: "bar",
		})
		killCursorsRes := mtest.CreateCommandErrorResponse(mtest.CommandError{
			Code:    errorInterrupted,
			Name:    "foo",
			Message: "bar",
		})
		changeDoc := bson.D{{"_id", bson.D{{"x", 1}}}}
		resumedAggRes := mtest.CreateCursorResponse(1, ns, mtest.FirstBatch, changeDoc)
		mt.AddMockResponses(aggRes, getMoreRes, killCursorsRes, resumedAggRes)

		cs, err := mt.Coll.Watch(mtest.Background, mongo.Pipeline{})
		assert.Nil(mt, err, "Watch error: %v", err)
		defer closeStream(cs)

		assert.True(mt, cs.Next(mtest.Background), "expected Next to return true, got false")
		assert.Nil(mt, cs.Err(), "change stream error: %v", cs.Err())
	})

	startAtOpTimeOpts := mtest.NewOptions().MinServerVersion("4.0").MaxServerVersion("4.0.6")
	mt.RunOpts("include startAtOperationTime", startAtOpTimeOpts, func(mt *mtest.T) {
		// $changeStream stage for ChangeStream against a server >=4.0 and <4.0.7 that has not received any results yet
		// MUST include a startAtOperationTime option when resuming a changestream.

		cs, err := mt.Coll.Watch(mtest.Background, mongo.Pipeline{})
		assert.Nil(mt, err, "Watch error: %v", err)
		defer closeStream(cs)

		generateEvents(mt, 1)
		// kill cursor to force resumable error
		killChangeStreamCursor(mt, cs)

		mt.ClearEvents()
		// change stream should resume once and get new change
		assert.True(mt, cs.Next(mtest.Background), "expected Next to return true, got false")
		// Next should cause getMore, killCursors, and aggregate to run
		assert.NotNil(mt, mt.GetStartedEvent(), "expected getMore event, got nil")
		assert.NotNil(mt, mt.GetStartedEvent(), "expected killCursors event, got nil")
		aggEvent := mt.GetStartedEvent()
		assert.NotNil(mt, aggEvent, "expected aggregate event, got nil")
		assert.Equal(mt, "aggregate", aggEvent.CommandName, "expected command name 'aggregate', got '%v'", aggEvent.CommandName)

		// check for startAtOperationTime in pipeline
		csStage := aggEvent.Command.Lookup("pipeline").Array().Index(0).Value().Document() // $changeStream stage
		_, err = csStage.Lookup("$changeStream").Document().LookupErr("startAtOperationTime")
		assert.Nil(mt, err, "startAtOperationTime not included in aggregate command")
	})
	mt.RunOpts("decode does not panic", noClientOpts, func(mt *mtest.T) {
		testCases := []struct {
			name             string
			st               streamType
			minServerVersion string
		}{
			{"client", client, "4.0"},
			{"database", database, "4.0"},
			{"collection", collection, ""},
		}
		for _, tc := range testCases {
			tcOpts := mtest.NewOptions()
			if tc.minServerVersion != "" {
				tcOpts.MinServerVersion(tc.minServerVersion)
			}
			mt.RunOpts(tc.name, tcOpts, func(mt *mtest.T) {
				var cs *mongo.ChangeStream
				var err error
				switch tc.st {
				case client:
					cs, err = mt.Client.Watch(mtest.Background, mongo.Pipeline{})
				case database:
					cs, err = mt.DB.Watch(mtest.Background, mongo.Pipeline{})
				case collection:
					cs, err = mt.Coll.Watch(mtest.Background, mongo.Pipeline{})
				}
				assert.Nil(mt, err, "Watch error: %v", err)
				defer closeStream(cs)

				generateEvents(mt, 1)
				assert.True(mt, cs.Next(mtest.Background), "expected Next true, got false")
				var res bson.D
				err = cs.Decode(&res)
				assert.Nil(mt, err, "Decode error: %v", err)
				assert.True(mt, len(res) > 0, "expected non-empty document, got empty")
			})
		}
	})
	mt.Run("resume error advances cursor", func(mt *mtest.T) {
		// Underlying cursor is advanced after a resumeable error occurs

		cs, err := mt.Coll.Watch(mtest.Background, mongo.Pipeline{})
		assert.Nil(mt, err, "Watch error: %v", err)
		defer closeStream(cs)

		ensureResumeToken(mt, cs)
		killChangeStreamCursor(mt, cs)
		ensureResumeToken(mt, cs)
	})
	mt.Run("maxAwaitTimeMS", func(mt *mtest.T) {
		// maxAwaitTimeMS option should be sent as maxTimeMS on getMore

		opts := options.ChangeStream().SetMaxAwaitTime(100 * time.Millisecond)
		cs, err := mt.Coll.Watch(mtest.Background, mongo.Pipeline{}, opts)
		assert.Nil(mt, err, "Watch error: %v", err)
		defer closeStream(cs)

		_, err = mt.Coll.InsertOne(mtest.Background, bson.D{{"x", 1}})
		assert.Nil(mt, err, "InsertOne error: %v", err)
		mt.ClearEvents()
		assert.True(mt, cs.Next(mtest.Background), "expected Next true, got false")

		e := mt.GetStartedEvent()
		assert.NotNil(mt, e, "expected getMore event, got nil")
		_, err = e.Command.LookupErr("maxTimeMS")
		assert.Nil(mt, err, "field maxTimeMS not found in command %v", e.Command)
	})
	mt.RunOpts("resume token", noClientOpts, func(mt *mtest.T) {
		// Prose tests to make assertions on resume tokens for change streams that have not done a getMore yet
		mt.RunOpts("no getMore", noClientOpts, func(mt *mtest.T) {
			pbrtOpts := mtest.NewOptions().MinServerVersion(minPbrtVersion).CreateClient(false)
			mt.RunOpts("with PBRT support", pbrtOpts, func(mt *mtest.T) {
				testCases := []struct {
					name             string
					rt               resumeType
					minServerVersion string
				}{
					{"startAfter", startAfter, minStartAfterVersion},
					{"resumeAfter", resumeAfter, minPbrtVersion},
					{"neither", operationTime, minPbrtVersion},
				}

				for _, tc := range testCases {
					tcOpts := mtest.NewOptions().MinServerVersion(tc.minServerVersion)
					mt.RunOpts(tc.name, tcOpts, func(mt *mtest.T) {
						// create temp stream to get a resume token
						mt.ClearEvents()
						cs, err := mt.Coll.Watch(mtest.Background, mongo.Pipeline{})
						assert.Nil(mt, err, "Watch error: %v", err)

						// Initial resume token should equal the PBRT in the aggregate command
						pbrt, opTime := getAggregateResponseInfo(mt)
						compareResumeTokens(mt, cs, pbrt)

						numEvents := 5
						generateEvents(mt, numEvents)

						// Iterate over one event to get resume token
						assert.True(mt, cs.Next(mtest.Background), "expected Next to return true, got false")
						token := cs.ResumeToken()
						closeStream(cs)

						var numExpectedEvents int
						var initialToken bson.Raw
						var opts *options.ChangeStreamOptions
						switch tc.rt {
						case startAfter:
							numExpectedEvents = numEvents - 1
							initialToken = token
							opts = options.ChangeStream().SetStartAfter(token)
						case resumeAfter:
							numExpectedEvents = numEvents - 1
							initialToken = token
							opts = options.ChangeStream().SetResumeAfter(token)
						case operationTime:
							numExpectedEvents = numEvents
							opts = options.ChangeStream().SetStartAtOperationTime(&opTime)
						}

						// clear slate and create new change stream
						mt.ClearEvents()
						cs, err = mt.Coll.Watch(mtest.Background, mongo.Pipeline{}, opts)
						assert.Nil(mt, err, "Watch error: %v", err)
						defer closeStream(cs)

						aggPbrt, _ := getAggregateResponseInfo(mt)
						compareResumeTokens(mt, cs, initialToken)

						for i := 0; i < numExpectedEvents; i++ {
							assert.True(mt, cs.Next(mtest.Background), "expected Next to return true, got false")
							// while we're not at the last doc in the batch, the resume token should be the _id of the
							// document
							if i != numExpectedEvents-1 {
								compareResumeTokens(mt, cs, cs.Current.Lookup("_id").Document())
							}
						}
						// at end of batch, the resume token should equal the PBRT of the aggregate
						compareResumeTokens(mt, cs, aggPbrt)
					})
				}
			})

			noPbrtOpts := mtest.NewOptions().MaxServerVersion("4.0.6")
			mt.RunOpts("without PBRT support", noPbrtOpts, func(mt *mtest.T) {
				collName := mt.Coll.Name()
				dbName := mt.Coll.Database().Name()
				cs, err := mt.Coll.Watch(mtest.Background, mongo.Pipeline{})
				assert.Nil(mt, err, "Watch error: %v", err)
				defer closeStream(cs)

				compareResumeTokens(mt, cs, nil) // should be no resume token because no PBRT
				numEvents := 5
				generateEvents(mt, numEvents)
				// iterate once to get a resume token
				assert.True(mt, cs.Next(mtest.Background), "expected Next to return true, got false")
				token := cs.ResumeToken()
				assert.NotNil(mt, token, "expected resume token, got nil")

				testCases := []struct {
					name            string
					opts            *options.ChangeStreamOptions
					iterateStream   bool // whether or not resulting change stream should be iterated
					initialToken    bson.Raw
					numDocsExpected int
				}{
					{"resumeAfter", options.ChangeStream().SetResumeAfter(token), true, token, numEvents - 1},
					{"no options", nil, false, nil, 0},
				}
				for _, tc := range testCases {
					mt.Run(tc.name, func(mt *mtest.T) {
						coll := mt.Client.Database(dbName).Collection(collName)
						cs, err := coll.Watch(mtest.Background, mongo.Pipeline{}, tc.opts)
						assert.Nil(mt, err, "Watch error: %v", err)
						defer closeStream(cs)

						compareResumeTokens(mt, cs, tc.initialToken)
						if !tc.iterateStream {
							return
						}

						for i := 0; i < tc.numDocsExpected; i++ {
							assert.True(mt, cs.Next(mtest.Background), "expected Next to return true, got false")
							// current resume token should always equal _id of current document
							compareResumeTokens(mt, cs, cs.Current.Lookup("_id").Document())
						}
					})
				}
			})
		})
	})
}

func closeStream(cs *mongo.ChangeStream) {
	_ = cs.Close(mtest.Background)
}

func generateEvents(mt *mtest.T, numEvents int) {
	mt.Helper()

	for i := 0; i < numEvents; i++ {
		doc := bson.D{{"x", i}}
		_, err := mt.Coll.InsertOne(mtest.Background, doc)
		assert.Nil(mt, err, "InsertOne error on document %v: %v", doc, err)
	}
}

func killChangeStreamCursor(mt *mtest.T, cs *mongo.ChangeStream) {
	mt.Helper()

	db := mt.Coll.Database().Client().Database("admin")
	err := db.RunCommand(mtest.Background, bson.D{
		{"killCursors", mt.Coll.Name()},
		{"cursors", bson.A{cs.ID()}},
	}).Err()
	assert.Nil(mt, err, "killCursors error: %v", err)
}

// returns pbrt, operationTime from aggregate command response
func getAggregateResponseInfo(mt *mtest.T) (bson.Raw, primitive.Timestamp) {
	mt.Helper()

	succeeded := mt.GetSucceededEvent()
	assert.NotNil(mt, succeeded, "expected success event for aggregate, got nil")
	assert.Equal(mt, "aggregate", succeeded.CommandName, "expected command name 'aggregate', got '%v'", succeeded.CommandName)

	pbrt := succeeded.Reply.Lookup("cursor", "postBatchResumeToken").Document()
	optimeT, optimeI := succeeded.Reply.Lookup("operationTime").Timestamp()
	return pbrt, primitive.Timestamp{T: optimeT, I: optimeI}
}

func compareResumeTokens(mt *mtest.T, cs *mongo.ChangeStream, expected bson.Raw) {
	mt.Helper()
	assert.Equal(mt, expected, cs.ResumeToken(), "expected resume token %v, got %v", expected, cs.ResumeToken())
}

func ensureResumeToken(mt *mtest.T, cs *mongo.ChangeStream) {
	mt.Helper()

	_, err := mt.Coll.InsertOne(mtest.Background, bson.D{
		{"ensureResumeToken", 1},
	})
	assert.Nil(mt, err, "InsertOne error for ensureResumeToken doc: %v", err)
	assert.True(mt, cs.Next(mtest.Background), "expected cs.Next to return true, got false")
}
