// Package mongorestore writes BSON data to a MongoDB instance.
package mongorestore

import (
	"fmt"
	"github.com/mongodb/mongo-tools/common/auth"
	"github.com/mongodb/mongo-tools/common/db"
	"github.com/mongodb/mongo-tools/common/intents"
	"github.com/mongodb/mongo-tools/common/log"
	"github.com/mongodb/mongo-tools/common/options"
	"github.com/mongodb/mongo-tools/common/progress"
	"github.com/mongodb/mongo-tools/common/util"
	"gopkg.in/mgo.v2"
	"gopkg.in/mgo.v2/bson"
	"sync"
)

type MongoRestore struct {
	ToolOptions   *options.ToolOptions
	InputOptions  *InputOptions
	OutputOptions *OutputOptions

	SessionProvider *db.SessionProvider

	TargetDirectory string

	tempUsersCol string
	tempRolesCol string

	// other internal state
	manager         *intents.Manager
	safety          *mgo.Safe
	progressManager *progress.Manager

	objCheck     bool
	oplogLimit   bson.MongoTimestamp
	useStdin     bool
	isMongos     bool
	authVersions authVersionPair

	// a map of database names to a list of collection names
	knownCollections      map[string][]string
	knownCollectionsMutex sync.Mutex
}

func (restore *MongoRestore) ParseAndValidateOptions() error {
	// Can't use option pkg defaults for --objcheck because it's two separate flags,
	// and we need to be able to see if they're both being used. We default to
	// true here and then see if noobjcheck is enabled.
	log.Log(log.DebugHigh, "checking options")
	if restore.InputOptions.Objcheck {
		restore.objCheck = true
		log.Log(log.DebugHigh, "\tdumping with object check enabled")
	} else {
		log.Log(log.DebugHigh, "\tdumping with object check disabled")
	}

	if restore.ToolOptions.DB == "" && restore.ToolOptions.Collection != "" {
		return fmt.Errorf("cannot dump a collection without a specified database")
	}

	if restore.ToolOptions.DB != "" {
		if err := util.ValidateDBName(restore.ToolOptions.DB); err != nil {
			return fmt.Errorf("invalid db name: %v", err)
		}
	}
	if restore.ToolOptions.Collection != "" {
		if err := util.ValidateCollectionGrammar(restore.ToolOptions.Collection); err != nil {
			return fmt.Errorf("invalid collection name: %v", err)
		}
	}
	if restore.InputOptions.RestoreDBUsersAndRoles && restore.ToolOptions.DB == "" {
		return fmt.Errorf("cannot use --restoreDbUsersAndRoles without a specified database")
	}
	if restore.InputOptions.RestoreDBUsersAndRoles && restore.ToolOptions.DB == "admin" {
		return fmt.Errorf("cannot use --restoreDbUsersAndRoles with the admin database")
	}

	var err error
	restore.isMongos, err = restore.SessionProvider.IsMongos()
	if err != nil {
		return err
	}
	if restore.isMongos {
		log.Log(log.DebugLow, "restoring to a sharded system")
		if restore.ToolOptions.DB == "" {
			return fmt.Errorf("cannot do a full restore on a sharded system")
		}
	}

	if restore.InputOptions.OplogLimit != "" {
		if !restore.InputOptions.OplogReplay {
			return fmt.Errorf("cannot use --oplogLimit without --oplogReplay enabled")
		}
		restore.oplogLimit, err = ParseTimestampFlag(restore.InputOptions.OplogLimit)
		if err != nil {
			return fmt.Errorf("error parsing timestamp argument to --oplogLimit: %v", err)
		}
	}

	// check if we are using a replica set and fall back to w=1 if we aren't (for <= 2.4)
	isRepl, err := restore.SessionProvider.IsReplicaSet()
	if err != nil {
		return fmt.Errorf("error determining if connected to replica set: %v", err)
	}
	restore.safety, err = db.BuildWriteConcern(restore.OutputOptions.WriteConcern, isRepl)
	if err != nil {
		return fmt.Errorf("error parsing write concern: %v", err)
	}

	// handle the hidden auth collection flags
	if restore.ToolOptions.HiddenOptions.TempUsersColl == nil {
		restore.tempUsersCol = "tempusers"
	} else {
		restore.tempUsersCol = *restore.ToolOptions.HiddenOptions.TempUsersColl
	}
	if restore.ToolOptions.HiddenOptions.TempRolesColl == nil {
		restore.tempRolesCol = "temproles"
	} else {
		restore.tempRolesCol = *restore.ToolOptions.HiddenOptions.TempRolesColl
	}

	if restore.ToolOptions.HiddenOptions.BulkWriters < 0 {
		return fmt.Errorf(
			"cannot specify a negative number of insertion workers per collection")
	}

	// a single dash signals reading from stdin
	if restore.TargetDirectory == "-" {
		restore.useStdin = true
		if restore.ToolOptions.Collection == "" {
			return fmt.Errorf("cannot restore from stdin without a specified collection")
		}
	}

	return nil
}

func (restore *MongoRestore) Restore() error {
	err := restore.ParseAndValidateOptions()
	if err != nil {
		log.Logf(log.DebugLow, "got error from options parsing: %v", err)
		return err
	}

	// Build up all intents to be restored
	restore.manager = intents.NewCategorizingIntentManager()

	// handle cases where the user passes in a file instead of a directory
	if isBSON(restore.TargetDirectory) {
		log.Log(log.DebugLow, "mongorestore target is a file, not a directory")
		err = restore.handleBSONInsteadOfDirectory(restore.TargetDirectory)
		if err != nil {
			return err
		}
	} else {
		log.Log(log.DebugLow, "mongorestore target is a directory, not a file")
	}

	switch {
	case restore.ToolOptions.DB == "" && restore.ToolOptions.Collection == "":
		log.Logf(log.Always,
			"building a list of dbs and collections to restore from %v dir",
			restore.TargetDirectory)
		err = restore.CreateAllIntents(restore.TargetDirectory)
	case restore.ToolOptions.DB != "" && restore.ToolOptions.Collection == "":
		log.Logf(log.Always,
			"building a list of collections to restore from %v dir",
			restore.TargetDirectory)
		err = restore.CreateIntentsForDB(
			restore.ToolOptions.DB,
			restore.TargetDirectory)
	case restore.ToolOptions.DB != "" && restore.ToolOptions.Collection != "":
		log.Logf(log.Always, "checking for collection data in %v", restore.TargetDirectory)
		err = restore.CreateIntentForCollection(
			restore.ToolOptions.DB,
			restore.ToolOptions.Collection,
			restore.TargetDirectory)
	}
	if err != nil {
		return fmt.Errorf("error scanning filesystem: %v", err)
	}

	// If restoring users and roles, make sure we validate auth versions
	if restore.ShouldRestoreUsersAndRoles() {
		log.Log(log.Info, "comparing auth version of the dump directory and target server")
		restore.authVersions.Dump, err = restore.GetDumpAuthVersion()
		if err != nil {
			return fmt.Errorf("error getting auth version from dump: %v", err)
		}
		restore.authVersions.Server, err = auth.GetAuthVersion(restore.SessionProvider)
		if err != nil {
			return fmt.Errorf("error getting auth version of server: %v", err)
		}
		err = restore.ValidateAuthVersions()
		if err != nil {
			return fmt.Errorf(
				"the users and roles collections in the dump have an incompatible auth version with target server: %v",
				err)
		}
	}

	// Restore the regular collections
	if restore.OutputOptions.NumParallelCollections > 0 {
		restore.manager.Finalize(intents.MultiDatabaseLTF)
	} else {
		// use legacy restoration order if we are single-threaded
		restore.manager.Finalize(intents.Legacy)
	}
	err = restore.RestoreIntents()
	if err != nil {
		return fmt.Errorf("restore error: %v", err)
	}

	// Restore users/roles
	if restore.ShouldRestoreUsersAndRoles() {
		if restore.manager.Users() != nil {
			err = restore.RestoreUsersOrRoles(Users, restore.manager.Users())
			if err != nil {
				return fmt.Errorf("restore error: %v", err)
			}
		}
		if restore.manager.Roles() != nil {
			err = restore.RestoreUsersOrRoles(Roles, restore.manager.Roles())
			if err != nil {
				return fmt.Errorf("restore error: %v", err)
			}
		}
	}

	// Restore oplog
	if restore.InputOptions.OplogReplay {
		err = restore.RestoreOplog()
		if err != nil {
			return fmt.Errorf("restore error: %v", err)
		}
	}

	log.Log(log.Always, "done")
	return nil
}
