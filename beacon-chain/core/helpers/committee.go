// Package helpers contains helper functions outlined in ETH2.0 spec:
// https://github.com/ethereum/eth2.0-specs/blob/master/specs/core/0_beacon-chain.md#helper-functions
package helpers

import (
	"encoding/binary"
	"errors"
	"fmt"
	"sort"

	"github.com/prysmaticlabs/prysm/beacon-chain/cache"
	"github.com/prysmaticlabs/prysm/beacon-chain/utils"
	pb "github.com/prysmaticlabs/prysm/proto/beacon/p2p/v1"
	"github.com/prysmaticlabs/prysm/shared/bitutil"
	"github.com/prysmaticlabs/prysm/shared/bytesutil"
	"github.com/prysmaticlabs/prysm/shared/mathutil"
	"github.com/prysmaticlabs/prysm/shared/params"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

var committeeCache = cache.NewCommitteesCache()

// CrosslinkCommittee defines the validator committee of slot and shard combinations.
type CrosslinkCommittee struct {
	Committee []uint64
	Shard     uint64
}

// EpochCommitteeCount returns the number of crosslink committees of an epoch.
//
// Spec pseudocode definition:
//   def get_epoch_committee_count(state: BeaconState, epoch: Epoch) -> int:
//    """
//    Return the number of committees in one epoch.
//    """
//    active_validators = get_active_validator_indices(state, epoch)
//    return max(
//        1,
//        min(
//            SHARD_COUNT // SLOTS_PER_EPOCH,
//            len(active_validators) // SLOTS_PER_EPOCH // TARGET_COMMITTEE_SIZE,
//        )
//    ) * SLOTS_PER_EPOCH
func EpochCommitteeCount(state *pb.BeaconState, epoch uint64) uint64 {

	minCommitteePerSlot := uint64(1)
	activeIndices := ActiveValidatorIndices(state, epoch)
	// Max committee count per slot will be 0 when shard count is less than epoch length, this
	// covers the special case to ensure there's always 1 max committee count per slot.
	var committeeSizesPerSlot = minCommitteePerSlot
	if params.BeaconConfig().ShardCount/params.BeaconConfig().SlotsPerEpoch > minCommitteePerSlot {
		committeeSizesPerSlot = params.BeaconConfig().ShardCount / params.BeaconConfig().SlotsPerEpoch
	}

	var currCommitteePerSlot = activeValidatorCount / params.BeaconConfig().SlotsPerEpoch / params.BeaconConfig().TargetCommitteeSize

	if currCommitteePerSlot > committeeSizesPerSlot {
		return committeeSizesPerSlot * params.BeaconConfig().SlotsPerEpoch
	}
	if currCommitteePerSlot < 1 {
		return minCommitteePerSlot * params.BeaconConfig().SlotsPerEpoch
	}
	return currCommitteePerSlot * params.BeaconConfig().SlotsPerEpoch
}

// CrosslinkCommitteeAtEpoch returns the crosslink committee of a given epoch.
//
// Spec pseudocode definition:
//  def get_crosslink_committee(state: BeaconState, epoch: Epoch, shard: Shard) -> List[ValidatorIndex]:
//    return compute_committee(
//        indices=get_active_validator_indices(state, epoch),
//        seed=generate_seed(state, epoch),
//        index=(shard + SHARD_COUNT - get_epoch_start_shard(state, epoch)) % SHARD_COUNT,
//        count=get_epoch_committee_count(state, epoch),
//    )
func CrosslinkCommitteeAtEpoch(state *pb.BeaconState, epoch uint64, shard uint64) ([]uint64, error) {
	indices := ActiveValidatorIndices(state, epoch)
	seed, err := GenerateSeed(state, epoch)
	if err != nil {
		return nil, fmt.Errorf("could not generate seed: %v", err)
	}
	startShard, err := EpochStartShard(state, epoch)
	if err != nil {
		return nil, fmt.Errorf("could not get start shard: %v", err)
	}
	shardCount := params.BeaconConfig().ShardCount
	currentShard := (shard + shardCount - startShard) % shardCount
	committeeCount := EpochCommitteeCount(state, epoch)
	return ComputeCommittee(indices, seed, currentShard, committeeCount)
}

// ComputeCommittee returns the requested shuffled committee out of the total committees using
// validator indices and seed.
//
// Spec pseudocode definition:
//  def compute_committee(indices: List[ValidatorIndex], seed: Bytes32, index: int, count: int) -> List[ValidatorIndex]:
//    start = (len(indices) * index) // count
//    end = (len(indices) * (index + 1)) // count
//    return [indices[get_shuffled_index(i, len(indices), seed)] for i in range(start, end)]
func ComputeCommittee(
	validatorIndices []uint64,
	seed [32]byte,
	index uint64,
	totalCommittees uint64,
) ([]uint64, error) {
	validatorCount := uint64(len(validatorIndices))
	startOffset := utils.SplitOffset(validatorCount, totalCommittees, index)
	endOffset := utils.SplitOffset(validatorCount, totalCommittees, index+1)

	indices := make([]uint64, endOffset-startOffset)
	for i := startOffset; i < endOffset; i++ {
		permutedIndex, err := utils.PermutedIndex(i, validatorCount, seed)
		if err != nil {
			return []uint64{}, fmt.Errorf("could not get permuted index at index %d: %v", i, err)
		}
		indices[i-startOffset] = validatorIndices[permutedIndex]
	}
	return indices, nil
}

// Shuffling shuffles input validator indices and splits them by slot and shard.
//
// Spec pseudocode definition:
//   def get_shuffling(seed: Bytes32,
//                  validators: List[Validator],
//                  epoch: Epoch) -> List[List[ValidatorIndex]]
//    """
//    Shuffle ``validators`` into crosslink committees seeded by ``seed`` and ``epoch``.
//    Return a list of ``committees_per_epoch`` committees where each
//    committee is itself a list of validator indices.
//    """
//
//    active_validator_indices = get_active_validator_indices(validators, epoch)
//
//    committees_per_epoch = get_epoch_committee_count(len(active_validator_indices))
//
//    # Shuffle
//    seed = xor(seed, int_to_bytes32(epoch))
//    shuffled_active_validator_indices = shuffle(active_validator_indices, seed)
//
//    # Split the shuffled list into committees_per_epoch pieces
//    return split(shuffled_active_validator_indices, committees_per_epoch)
func Shuffling(
	seed [32]byte,
	validators []*pb.Validator,
	epoch uint64) ([][]uint64, error) {

	// Figure out how many committees can be in a single epoch.
	s := &pb.BeaconState{ValidatorRegistry: validators}
	activeIndices := ActiveValidatorIndices(s, epoch)
	committeesPerEpoch := EpochCommitteeCount(s, epoch)

	// Convert slot to bytes and xor it with seed.
	epochInBytes := make([]byte, 32)
	binary.LittleEndian.PutUint64(epochInBytes, epoch)
	seed = bytesutil.ToBytes32(bytesutil.Xor(seed[:], epochInBytes))

	shuffledIndices, err := utils.ShuffleIndices(seed, activeIndices)
	if err != nil {
		return nil, err
	}

	// Split the shuffled list into epoch_length * committees_per_slot pieces.
	return utils.SplitIndices(shuffledIndices, committeesPerEpoch), nil
}

// AttestingIndices returns the attesting participants indices.
//
// Spec pseudocode definition:
//   def get_attesting_indices(state: BeaconState,
//                          attestation_data: AttestationData,
//                          bitfield: bytes) -> List[ValidatorIndex]:
//    """
//    Return the sorted attesting indices corresponding to ``attestation_data`` and ``bitfield``.
//    """
//    committee = get_crosslink_committee(state, attestation_data.target_epoch, attestation_data.crosslink.shard)
//    assert verify_bitfield(bitfield, len(committee))
//    return sorted([index for i, index in enumerate(committee) if get_bitfield_bit(bitfield, i) == 0b1])
func AttestingIndices(state *pb.BeaconState, data *pb.AttestationData, bitfield []byte) ([]uint64, error) {
	committee, err := CrosslinkCommitteeAtEpoch(state, data.TargetEpoch, data.Shard)
	if err != nil {
		return nil, fmt.Errorf("could not get committee: %v", err)
	}
	if isValidated, err := VerifyBitfield(bitfield, len(committee)); !isValidated || err != nil {
		if err != nil {
			return nil, err
		}
		return nil, errors.New("bitfield is unable to be verified")
	}
	sort.Slice(committee, func(i, j int) bool { return committee[i] < committee[j] })
	return committee, nil
}

// VerifyBitfield validates a bitfield with a given committee size.
//
// Spec pseudocode definition:
//   def verify_bitfield(bitfield: bytes, committee_size: int) -> bool:
//     """
//     Verify ``bitfield`` against the ``committee_size``.
//     """
//     if len(bitfield) != (committee_size + 7) // 8:
//         return False
//     # Check `bitfield` is padded with zero bits only
//     for i in range(committee_size, len(bitfield) * 8):
//         if get_bitfield_bit(bitfield, i) == 0b1:
//             return False
//     return True
func VerifyBitfield(bitfield []byte, committeeSize int) (bool, error) {
	if len(bitfield) != mathutil.CeilDiv8(committeeSize) {
		return false, fmt.Errorf(
			"wanted participants bitfield length %d, got: %d",
			mathutil.CeilDiv8(committeeSize),
			len(bitfield))
	}
	bitLength := len(bitfield) << 3
	for i := committeeSize; i < bitLength; i++ {
		set, err := bitutil.CheckBit(bitfield, i)
		if err != nil {
			return false, err
		}
		if set {
			return false, nil
		}
	}
	return true, nil
}

// CommitteeAssignment is used to query committee assignment from
// current and previous epoch.
//
// Spec pseudocode definition:
//   def get_committee_assignment(
//        state: BeaconState,
//        epoch: Epoch,
//        validator_index: ValidatorIndex,
//        registry_change: bool=False) -> Tuple[List[ValidatorIndex], Shard, Slot, bool]:
//    """
//    Return the committee assignment in the ``epoch`` for ``validator_index`` and ``registry_change``.
//    ``assignment`` returned is a tuple of the following form:
//        * ``assignment[0]`` is the list of validators in the committee
//        * ``assignment[1]`` is the shard to which the committee is assigned
//        * ``assignment[2]`` is the slot at which the committee is assigned
//        * ``assignment[3]`` is a bool signaling if the validator is expected to propose
//            a beacon block at the assigned slot.
//    """
//    previous_epoch = get_previous_epoch(state)
//    next_epoch = get_current_epoch(state)
//    assert previous_epoch <= epoch <= next_epoch
//
//    epoch_start_slot = get_epoch_start_slot(epoch)
//    for slot in range(epoch_start_slot, epoch_start_slot + SLOTS_PER_EPOCH):
//        crosslink_committees = get_crosslink_committees_at_slot(
//            state,
//            slot,
//            registry_change=registry_change,
//        )
//        selected_committees = [
//            committee  # Tuple[List[ValidatorIndex], Shard]
//            for committee in crosslink_committees
//            if validator_index in committee[0]
//        ]
//        if len(selected_committees) > 0:
//            validators = selected_committees[0][0]
//            shard = selected_committees[0][1]
//            first_committee_at_slot = crosslink_committees[0][0]  # List[ValidatorIndex]
//            is_proposer = first_committee_at_slot[slot % len(first_committee_at_slot)] == validator_index
//
//            assignment = (validators, shard, slot, is_proposer)
//            return assignment
func CommitteeAssignment(
	state *pb.BeaconState,
	slot uint64,
	validatorIndex uint64,
	registryChange bool) ([]uint64, uint64, uint64, bool, error) {
	var selectedCommittees []*cache.CommitteeInfo

	wantedEpoch := slot / params.BeaconConfig().SlotsPerEpoch
	prevEpoch := PrevEpoch(state)
	nextEpoch := NextEpoch(state)

	if wantedEpoch < prevEpoch || wantedEpoch > nextEpoch {
		return nil, 0, 0, false, fmt.Errorf(
			"epoch %d out of bounds: %d <= epoch <= %d",
			wantedEpoch-params.BeaconConfig().GenesisEpoch,
			prevEpoch-params.BeaconConfig().GenesisEpoch,
			nextEpoch-params.BeaconConfig().GenesisEpoch,
		)
	}

	var cachedCommittees *cache.CommitteesInSlot
	var err error
	startSlot := StartSlot(wantedEpoch)
	for slot := startSlot; slot < startSlot+params.BeaconConfig().SlotsPerEpoch; slot++ {

		cachedCommittees, err = committeeCache.CommitteesInfoBySlot(slot)
		if err != nil {
			return []uint64{}, 0, 0, false, err
		}
		if cachedCommittees == nil {
			crosslinkCommittees := []*CrosslinkCommittee{}
			cachedCommittees = ToCommitteeCache(slot, crosslinkCommittees)
			if err := committeeCache.AddCommittees(cachedCommittees); err != nil {
				return []uint64{}, 0, 0, false, err
			}
		}
		for _, committee := range cachedCommittees.Committees {
			for _, idx := range committee.Committee {
				if idx == validatorIndex {
					selectedCommittees = append(selectedCommittees, committee)
				}

				if len(selectedCommittees) > 0 {
					validators := selectedCommittees[0].Committee
					shard := selectedCommittees[0].Shard
					firstCommitteeAtSlot := cachedCommittees.Committees[0].Committee
					isProposer := firstCommitteeAtSlot[slot%
						uint64(len(firstCommitteeAtSlot))] == validatorIndex
					return validators, shard, slot, isProposer, nil
				}
			}
		}
	}
	return []uint64{}, 0, 0, false, status.Error(codes.NotFound, "validator not found found in assignments")
}

// ShardDelta returns the minimum number of shards get processed in one epoch.
//
// Spec pseudocode definition:
// 	def get_shard_delta(state: BeaconState, epoch: Epoch) -> int:
//    return min(get_epoch_committee_count(state, epoch), SHARD_COUNT - SHARD_COUNT // SLOTS_PER_EPOCH)
func ShardDelta(beaconState *pb.BeaconState, epoch uint64) uint64 {
	shardCount := params.BeaconConfig().ShardCount
	minShardDelta := shardCount - shardCount/params.BeaconConfig().SlotsPerEpoch
	if EpochCommitteeCount(beaconState, epoch) < minShardDelta {
		return EpochCommitteeCount(beaconState, epoch)
	}
	return minShardDelta
}

// RestartCommitteeCache restarts the committee cache from scratch.
func RestartCommitteeCache() {
	committeeCache = cache.NewCommitteesCache()
}

// ToCommitteeCache converts crosslink committee object
// into a cache format, to be saved in cache.
func ToCommitteeCache(slot uint64, crosslinkCommittees []*CrosslinkCommittee) *cache.CommitteesInSlot {
	var cacheCommittee []*cache.CommitteeInfo
	for _, crosslinkCommittee := range crosslinkCommittees {
		cacheCommittee = append(cacheCommittee, &cache.CommitteeInfo{
			Committee: crosslinkCommittee.Committee,
			Shard:     crosslinkCommittee.Shard,
		})
	}
	committees := &cache.CommitteesInSlot{
		Slot:       slot,
		Committees: cacheCommittee,
	}
	return committees
}
