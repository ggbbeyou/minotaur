package builtin

import (
	"encoding/json"
	"github.com/kercylan98/minotaur/game"
	"github.com/kercylan98/minotaur/utils/generic"
	"github.com/kercylan98/minotaur/utils/synchronization"
)

// NewRankingList 创建一个排行榜
func NewRankingList[CompetitorID comparable, Score generic.Ordered](options ...RankingListOption[CompetitorID, Score]) *RankingList[CompetitorID, Score] {
	rankingList := &RankingList[CompetitorID, Score]{
		rankCount:   100,
		competitors: synchronization.NewMap[CompetitorID, Score](),
	}
	for _, option := range options {
		option(rankingList)
	}
	return rankingList
}

type RankingList[CompetitorID comparable, Score generic.Ordered] struct {
	asc         bool
	rankCount   int
	competitors *synchronization.Map[CompetitorID, Score]
	scores      []*scoreItem[CompetitorID, Score] // CompetitorID, Score

	rankChangeEventHandles []game.RankChangeEventHandle[CompetitorID, Score]
}

type scoreItem[CompetitorID comparable, Score generic.Ordered] struct {
	CompetitorId CompetitorID `json:"competitor_id,omitempty"`
	Score        Score        `json:"score,omitempty"`
}

func (slf *RankingList[CompetitorID, Score]) Competitor(competitorId CompetitorID, score Score) {
	v, exist := slf.competitors.GetExist(competitorId)
	if exist {
		if slf.Cmp(v, score) == 0 {
			return
		}
		rank, err := slf.GetRank(competitorId)
		if err != nil {
			return
		}
		slf.scores = append(slf.scores[0:rank], slf.scores[rank+1:]...)
		slf.competitors.Delete(competitorId)
		if slf.Cmp(score, v) > 0 {
			slf.competitor(competitorId, score, 0, rank-1)
		} else {
			slf.competitor(competitorId, score, rank, len(slf.scores)-1)
		}
		if len(slf.rankChangeEventHandles) > 0 {
			newRank, err := slf.GetRank(competitorId)
			if err != nil {
				panic(err)
			}
			slf.OnRankChangeEvent(competitorId, rank, newRank, v, score)
		}
	} else {
		if slf.rankCount > 0 && len(slf.scores) >= slf.rankCount {
			last := slf.scores[len(slf.scores)-1]
			if slf.Cmp(score, last.Score) <= 0 {
				return
			}
		}
		slf.competitor(competitorId, score, 0, len(slf.scores)-1)
		if len(slf.rankChangeEventHandles) > 0 {
			newRank, err := slf.GetRank(competitorId)
			if err != nil {
				panic(err)
			}
			slf.OnRankChangeEvent(competitorId, newRank, newRank, score, score)
		}
	}
}

func (slf *RankingList[CompetitorID, Score]) CompetitorIncrease(competitorId CompetitorID, score Score) {
	oldScore, err := slf.GetScore(competitorId)
	if err != nil {
		slf.Competitor(competitorId, score)
	} else {
		slf.Competitor(competitorId, oldScore+score)
	}
}

func (slf *RankingList[CompetitorID, Score]) RemoveCompetitor(competitorId CompetitorID) {
	if !slf.competitors.Exist(competitorId) {
		return
	}
	rank, err := slf.GetRank(competitorId)
	if err != nil {
		slf.competitors.Delete(competitorId)
		return
	}
	slf.scores = append(slf.scores[0:rank], slf.scores[rank+1:]...)
	slf.competitors.Delete(competitorId)
}

func (slf *RankingList[CompetitorID, Score]) Size() int {
	return slf.competitors.Size()
}

func (slf *RankingList[CompetitorID, Score]) GetRank(competitorId CompetitorID) (int, error) {
	competitorScore, exist := slf.competitors.GetExist(competitorId)
	if !exist {
		return 0, ErrRankingListNotExistCompetitor
	}

	low, high := 0, len(slf.scores)-1
	for low <= high {
		mid := (low + high) / 2
		data := slf.scores[mid]
		id, score := data.CompetitorId, data.Score
		if id == competitorId {
			return mid, nil
		} else if slf.Cmp(score, competitorScore) == 0 {
			for i := mid + 1; i <= high; i++ {
				data := slf.scores[i]
				if data.CompetitorId == competitorId {
					return i, nil
				}
			}
			for i := mid - 1; i >= low; i-- {
				data := slf.scores[i]
				if data.CompetitorId == competitorId {
					return i, nil
				}
			}
		} else if slf.Cmp(score, competitorScore) < 0 {
			high = mid - 1
		} else {
			low = mid + 1
		}
	}
	return 0, ErrRankingListIndexErr
}

func (slf *RankingList[CompetitorID, Score]) GetCompetitor(rank int) (competitorId CompetitorID, err error) {
	if rank < 0 || rank >= len(slf.scores) {
		return competitorId, ErrRankingListNonexistentRanking
	}
	return slf.scores[rank].CompetitorId, nil
}

func (slf *RankingList[CompetitorID, Score]) GetCompetitorWithRange(start, end int) ([]CompetitorID, error) {
	if start < 1 || end < start {
		return nil, ErrRankingListNonexistentRanking
	}
	total := len(slf.scores)
	if start > total {
		return nil, ErrRankingListNonexistentRanking
	}
	if end > total {
		end = total
	}
	var ids []CompetitorID
	for _, data := range slf.scores[start-1 : end] {
		ids = append(ids, data.CompetitorId)
	}
	return ids, nil
}

func (slf *RankingList[CompetitorID, Score]) GetScore(competitorId CompetitorID) (score Score, err error) {
	data, ok := slf.competitors.GetExist(competitorId)
	if !ok {
		return score, ErrRankingListNotExistCompetitor
	}
	return data, nil
}

func (slf *RankingList[CompetitorID, Score]) GetAllCompetitor() []CompetitorID {
	var result []CompetitorID
	for _, data := range slf.scores {
		result = append(result, data.CompetitorId)
	}
	return result
}

func (slf *RankingList[CompetitorID, Score]) Clear() {
	slf.competitors.Clear()
	slf.scores = make([]*scoreItem[CompetitorID, Score], 0)
}

func (slf *RankingList[CompetitorID, Score]) Cmp(s1, s2 Score) int {
	var result int
	if s1 > s2 {
		result = 1
	} else if s1 < s2 {
		result = -1
	} else {
		result = 0
	}
	if slf.asc {
		return -result
	} else {
		return result
	}
}

func (slf *RankingList[CompetitorID, Score]) competitor(competitorId CompetitorID, score Score, low, high int) {
	for low <= high {
		mid := (low + high) / 2
		data := slf.scores[mid]
		if slf.Cmp(data.Score, score) == 0 {
			for low = mid + 1; low <= high; low++ {
				if slf.Cmp(slf.scores[low].Score, score) != 0 {
					break
				}
			}
		} else if slf.Cmp(data.Score, score) < 0 {
			high = mid - 1
		} else {
			low = mid + 1
		}
	}

	count := len(slf.scores)
	if low == count {
		if slf.rankCount > 0 && count >= slf.rankCount {
			return
		}

		slf.scores = append(slf.scores, &scoreItem[CompetitorID, Score]{CompetitorId: competitorId, Score: score})
		slf.competitors.Set(competitorId, score)
		return
	}

	si := &scoreItem[CompetitorID, Score]{competitorId, score}

	//队首
	if low == 0 {
		slf.scores = append([]*scoreItem[CompetitorID, Score]{si}, slf.scores...)
	} else {
		tmp := append([]*scoreItem[CompetitorID, Score]{si}, slf.scores[low:]...)
		slf.scores = append(slf.scores[0:low], tmp...)
	}
	slf.competitors.Set(competitorId, score)
	if slf.rankCount <= 0 || len(slf.scores) <= slf.rankCount {
		return
	}

	count = len(slf.scores) - 1
	si = slf.scores[count]
	slf.competitors.Delete(si.CompetitorId)
	slf.scores = slf.scores[0:count]
}

func (slf *RankingList[CompetitorID, Score]) UnmarshalJSON(bytes []byte) error {
	var t struct {
		Competitors *synchronization.Map[CompetitorID, Score] `json:"competitors,omitempty"`
		Scores      []*scoreItem[CompetitorID, Score]         `json:"scores,omitempty"`
		Asc         bool                                      `json:"asc,omitempty"`
	}
	t.Competitors = synchronization.NewMap[CompetitorID, Score]()
	if err := json.Unmarshal(bytes, &t); err != nil {
		return err
	}
	slf.competitors = t.Competitors
	slf.scores = t.Scores
	slf.asc = t.Asc
	return nil
}

func (slf *RankingList[CompetitorID, Score]) MarshalJSON() ([]byte, error) {
	var t struct {
		Competitors *synchronization.Map[CompetitorID, Score] `json:"competitors,omitempty"`
		Scores      []*scoreItem[CompetitorID, Score]         `json:"scores,omitempty"`
		Asc         bool                                      `json:"asc,omitempty"`
	}
	t.Competitors = slf.competitors
	t.Scores = slf.scores
	t.Asc = slf.asc

	return json.Marshal(&t)
}

func (slf *RankingList[CompetitorID, Score]) RegRankChangeEvent(handle game.RankChangeEventHandle[CompetitorID, Score]) {
	slf.rankChangeEventHandles = append(slf.rankChangeEventHandles, handle)
}

func (slf *RankingList[CompetitorID, Score]) OnRankChangeEvent(competitorId CompetitorID, oldRank, newRank int, oldScore, newScore Score) {
	for _, handle := range slf.rankChangeEventHandles {
		handle(competitorId, oldRank, newRank, oldScore, newScore)
	}
}
