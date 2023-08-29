package worlds

import (
	"os"
	"slices"
	"time"

	"github.com/bedrock-tool/bedrocktool/locale"
	"github.com/bedrock-tool/bedrocktool/utils/behaviourpack"
	"github.com/df-mc/dragonfly/server/block/cube"
	"github.com/df-mc/dragonfly/server/world"
	"github.com/df-mc/dragonfly/server/world/chunk"
	"github.com/df-mc/dragonfly/server/world/mcdb"
	"github.com/df-mc/goleveldb/leveldb/opt"
	"github.com/sandertv/gophertunnel/minecraft"
	"github.com/sandertv/gophertunnel/minecraft/protocol"
	"github.com/sirupsen/logrus"
	"github.com/thomaso-mirodin/intmath/i32"
	"golang.org/x/exp/maps"
)

type worldStateInt interface {
	storeChunk(pos world.ChunkPos, dim world.Dimension, ch *chunk.Chunk, blockNBT map[cube.Pos]world.Block)
	storeEntity(id uint64, es *entityState)
	haveEntity(id uint64) bool
	getEntity(id uint64) (*entityState, bool)
	addEntityLink(el protocol.EntityLink)
}

type worldStateEnt struct {
	entities    map[uint64]*entityState
	entityLinks map[int64]map[int64]struct{}
}

func (w *worldStateEnt) storeEntity(id uint64, es *entityState) {
	w.entities[id] = es
}

func (w *worldStateEnt) haveEntity(id uint64) bool {
	_, ok := w.entities[id]
	return ok
}

func (w *worldStateEnt) getEntity(id uint64) (*entityState, bool) {
	e, ok := w.entities[id]
	return e, ok
}

func (w *worldStateEnt) addEntityLink(el protocol.EntityLink) {
	switch el.Type {
	case protocol.EntityLinkPassenger:
		fallthrough
	case protocol.EntityLinkRider:
		if _, ok := w.entityLinks[el.RiddenEntityUniqueID]; !ok {
			w.entityLinks[el.RiddenEntityUniqueID] = make(map[int64]struct{})
		}
		w.entityLinks[el.RiddenEntityUniqueID][el.RiderEntityUniqueID] = struct{}{}
	case protocol.EntityLinkRemove:
		delete(w.entityLinks[el.RiddenEntityUniqueID], el.RiderEntityUniqueID)
	}
}

type worldStateInternal struct {
	provider *mcdb.DB
	worldStateEnt
}

func (w *worldStateInternal) storeChunk(pos world.ChunkPos, dim world.Dimension, ch *chunk.Chunk, blockNBT map[cube.Pos]world.Block) {
	w.provider.StoreColumn(pos, dim, &world.Column{
		Chunk:         ch,
		BlockEntities: blockNBT,
	}, false)
}

func (w *worldStateInternal) saveEntities(exclude []string, dimension world.Dimension) error {
	chunkEntities := make(map[world.ChunkPos][]world.Entity)
	for _, es := range w.entities {
		if slices.Contains(exclude, es.EntityType) {
			continue
		}
		cp := world.ChunkPos{int32(es.Position.X()) >> 4, int32(es.Position.Z()) >> 4}
		links := maps.Keys(w.entityLinks[es.UniqueID])
		chunkEntities[cp] = append(chunkEntities[cp], es.ToServerEntity(links))
	}

	for cp, v := range chunkEntities {
		err := w.provider.StoreEntities(cp, dimension, v)
		if err != nil {
			logrus.Error(err)
		}
	}

	return nil
}

type worldStateDefer struct {
	chunks    map[world.ChunkPos]*chunk.Chunk
	blockNBTs map[world.ChunkPos]map[cube.Pos]world.Block
	worldStateEnt
}

func (w *worldStateDefer) storeChunk(pos world.ChunkPos, dim world.Dimension, ch *chunk.Chunk, blockNBT map[cube.Pos]world.Block) {
	w.chunks[pos] = ch
	w.blockNBTs[pos] = blockNBT
}

func (w *worldStateDefer) cullChunks() {
	for key, ch := range w.chunks {
		var empty = true
		for _, sub := range ch.Sub() {
			if !sub.Empty() {
				empty = false
				break
			}
		}
		if empty {
			delete(w.chunks, key)
		}
	}
}

func (w *worldStateDefer) ApplyTo(w2 worldStateInt, dimension world.Dimension, around cube.Pos, radius int32, cf func(world.ChunkPos, *chunk.Chunk)) {
	w.cullChunks()
	for cp, c := range w.chunks {
		dist := i32.Sqrt(i32.Pow(cp.X()-int32(around.X()/16), 2) + i32.Pow(cp.Z()-int32(around.Z()/16), 2))
		blockNBT := w.blockNBTs[cp]
		if dist <= radius {
			w2.storeChunk(cp, dimension, c, blockNBT)
			cf(cp, c)
		} else {
			cf(cp, nil)
		}
	}

	for k, es := range w.entities {
		x := int(es.Position[0])
		z := int(es.Position[2])
		dist := i32.Sqrt(i32.Pow(int32(x-around.X()), 2) + i32.Pow(int32(z-around.Z()), 2))
		if dist < radius*16 || w2.haveEntity(k) {
			w2.storeEntity(k, es)
		}
	}
}

type worldState struct {
	dimension          world.Dimension
	State              *worldStateInternal
	deferredState      *worldStateDefer
	openItemContainers map[byte]*itemContainer
	storedChunks       map[world.ChunkPos]bool
	useDeferred        bool

	excludeMobs []string
	VoidGen     bool
	timeSync    time.Time
	time        int
	Name        string
	folder      string
	provider    *mcdb.DB
}

func newWorldState(name string, folder string, dim world.Dimension) (*worldState, error) {
	if dim == nil {
		dim = world.Overworld
	}

	os.RemoveAll(folder)
	os.MkdirAll(folder, 0o777)
	provider, err := mcdb.Config{
		Log:         logrus.StandardLogger(),
		Compression: opt.DefaultCompression,
	}.Open(folder)
	if err != nil {
		return nil, err
	}

	return &worldState{
		dimension: dim,
		State: &worldStateInternal{
			provider: provider,
			worldStateEnt: worldStateEnt{
				entities:    make(map[uint64]*entityState),
				entityLinks: make(map[int64]map[int64]struct{}),
			},
		},
		openItemContainers: make(map[byte]*itemContainer),
		storedChunks:       make(map[world.ChunkPos]bool),
		Name:               name,
		folder:             folder,
		provider:           provider,
	}, nil
}

func (w *worldState) storeChunk(pos world.ChunkPos, ch *chunk.Chunk, blockNBT map[cube.Pos]world.Block) {
	w.storedChunks[pos] = true
	w.state().storeChunk(pos, w.dimension, ch, blockNBT)
}

func (w *worldState) initDeferred() {
	w.deferredState = &worldStateDefer{
		chunks:    make(map[world.ChunkPos]*chunk.Chunk),
		blockNBTs: make(map[world.ChunkPos]map[cube.Pos]world.Block),
		worldStateEnt: worldStateEnt{
			entities:    make(map[uint64]*entityState),
			entityLinks: make(map[int64]map[int64]struct{}),
		},
	}
}

func (w *worldState) state() worldStateInt {
	if w.useDeferred {
		return w.deferredState
	}
	return w.State
}

func (w *worldState) PauseCapture() {
	w.initDeferred()
	w.useDeferred = true
}

func (w *worldState) UnpauseCapture(around cube.Pos, radius int32, cf func(world.ChunkPos, *chunk.Chunk)) {
	w.deferredState.ApplyTo(w.State, w.dimension, around, radius, cf)
	w.useDeferred = false
	w.deferredState = nil
}

func (w *worldState) Finish(playerData map[string]any, spawn cube.Pos, gd minecraft.GameData, bp *behaviourpack.BehaviourPack) error {
	err := w.State.saveEntities(w.excludeMobs, w.dimension)
	if err != nil {
		return err
	}

	err = w.provider.SaveLocalPlayerData(playerData)
	if err != nil {
		return err
	}

	// write metadata
	s := w.provider.Settings()
	s.Spawn = spawn
	s.Name = w.Name

	// set gamerules
	ld := w.provider.LevelDat()
	ld.CheatsEnabled = true
	ld.RandomSeed = int64(gd.WorldSeed)
	for _, gr := range gd.GameRules {
		switch gr.Name {
		case "commandblockoutput":
			ld.CommandBlockOutput = gr.Value.(bool)
		case "maxcommandchainlength":
			ld.MaxCommandChainLength = int32(gr.Value.(uint32))
		case "commandblocksenabled":
			//ld.CommandsEnabled = gr.Value.(bool)
		case "dodaylightcycle":
			ld.DoDayLightCycle = gr.Value.(bool)
		case "doentitydrops":
			ld.DoEntityDrops = gr.Value.(bool)
		case "dofiretick":
			ld.DoFireTick = gr.Value.(bool)
		case "domobloot":
			ld.DoMobLoot = gr.Value.(bool)
		case "domobspawning":
			ld.DoMobSpawning = gr.Value.(bool)
		case "dotiledrops":
			ld.DoTileDrops = gr.Value.(bool)
		case "doweathercycle":
			ld.DoWeatherCycle = gr.Value.(bool)
		case "drowningdamage":
			ld.DrowningDamage = gr.Value.(bool)
		case "doinsomnia":
			ld.DoInsomnia = gr.Value.(bool)
		case "falldamage":
			ld.FallDamage = gr.Value.(bool)
		case "firedamage":
			ld.FireDamage = gr.Value.(bool)
		case "keepinventory":
			ld.KeepInventory = gr.Value.(bool)
		case "mobgriefing":
			ld.MobGriefing = gr.Value.(bool)
		case "pvp":
			ld.PVP = gr.Value.(bool)
		case "showcoordinates":
			ld.ShowCoordinates = gr.Value.(bool)
		case "naturalregeneration":
			ld.NaturalRegeneration = gr.Value.(bool)
		case "tntexplodes":
			ld.TNTExplodes = gr.Value.(bool)
		case "sendcommandfeedback":
			ld.SendCommandFeedback = gr.Value.(bool)
		case "randomtickspeed":
			ld.RandomTickSpeed = int32(gr.Value.(uint32))
		case "doimmediaterespawn":
			ld.DoImmediateRespawn = gr.Value.(bool)
		case "showdeathmessages":
			ld.ShowDeathMessages = gr.Value.(bool)
		case "functioncommandlimit":
			ld.FunctionCommandLimit = int32(gr.Value.(uint32))
		case "spawnradius":
			ld.SpawnRadius = int32(gr.Value.(uint32))
		case "showtags":
			ld.ShowTags = gr.Value.(bool)
		case "freezedamage":
			ld.FreezeDamage = gr.Value.(bool)
		case "respawnblocksexplode":
			ld.RespawnBlocksExplode = gr.Value.(bool)
		case "showbordereffect":
			ld.ShowBorderEffect = gr.Value.(bool)
		// todo
		default:
			logrus.Warnf(locale.Loc("unknown_gamerule", locale.Strmap{"Name": gr.Name}))
		}
	}

	// void world
	if w.VoidGen {
		ld.FlatWorldLayers = `{"biome_id":1,"block_layers":[{"block_data":0,"block_id":0,"count":1},{"block_data":0,"block_id":0,"count":2},{"block_data":0,"block_id":0,"count":1}],"encoding_version":3,"structure_options":null}`
		ld.Generator = 2
	}

	ld.RandomTickSpeed = 0
	s.CurrentTick = gd.Time

	ticksSince := int64(time.Since(w.timeSync)/time.Millisecond) / 50
	s.Time = int64(w.time)
	if ld.DoDayLightCycle {
		s.Time += ticksSince
		s.TimeCycle = true
	}

	if bp.HasContent() {
		if ld.Experiments == nil {
			ld.Experiments = map[string]any{}
		}
		ld.Experiments["data_driven_items"] = true
		ld.Experiments["experiments_ever_used"] = true
		ld.Experiments["saved_with_toggled_experiments"] = true
	}

	w.provider.SaveSettings(s)
	return w.provider.Close()
}
