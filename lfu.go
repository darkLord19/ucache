package gcache

import (
	"container/list"
	"time"
)

// LFUCache represent cache which discards the least frequently used items first
type LFUCache struct {
	baseCache
	items    map[interface{}]*lfuItem
	freqList *list.List // list for freqEntry
}

// NewLFU returns new LFU cache instance
func NewLFU(config Config) *LFUCache {
	return newLFUCache(config)
}

func newLFUCache(config Config) *LFUCache {
	c := &LFUCache{}
	buildCache(&c.baseCache, config)

	c.init()
	c.loadGroup.cache = c
	return c
}

func (c *LFUCache) init() {
	c.freqList = list.New()
	c.items = make(map[interface{}]*lfuItem, c.size+1)
	c.freqList.PushFront(&freqEntry{
		freq:  0,
		items: make(map[*lfuItem]struct{}),
	})
}

// Set a new key-value pair
func (c *LFUCache) Set(key, value interface{}) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	_, err := c.set(key, value)
	return err
}

// SetWithTTL Set a new key-value pair with an expiration time
func (c *LFUCache) SetWithTTL(key, value interface{}, expiration time.Duration) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	item, err := c.set(key, value)
	if err != nil {
		return err
	}

	t := c.clock.Now().Add(expiration)
	item.(*lfuItem).expiration = &t
	return nil
}

func (c *LFUCache) set(key, value interface{}) (interface{}, error) {
	var err error
	if c.serializeWith != nil {
		value, err = c.serializeWith(key, value)
		if err != nil {
			return nil, err
		}
	}

	// Check for existing item
	item, ok := c.items[key]
	if ok {
		item.value = value
	} else {
		// Verify size not exceeded
		if len(c.items) >= c.size {
			c.evict(1)
		}
		item = &lfuItem{
			clock:       c.clock,
			key:         key,
			value:       value,
			freqElement: nil,
		}
		el := c.freqList.Front()
		fe := el.Value.(*freqEntry)
		fe.items[item] = struct{}{}

		item.freqElement = el
		c.items[key] = item
	}

	if c.defaultTTL != nil {
		t := c.clock.Now().Add(*c.defaultTTL)
		item.expiration = &t
	}

	if c.onAdd != nil {
		c.onAdd(key, value)
	}

	return item, nil
}

// Get a value from cache pool using key if it exists.
// If it dose not exists key and has LoaderFunc,
// generate a value using `LoaderFunc` method returns value.
func (c *LFUCache) Get(key interface{}) (interface{}, error) {
	v, err := c.get(key, false)
	if err == ErrKeyNotFound {
		return c.getWithLoader(key, true)
	}
	return v, err
}

// GetIFPresent gets a value from cache pool using key if it exists.
// If it dose not exists key, returns ErrKeyNotFound.
// And send a request which refresh value for specified key if cache object has LoaderFunc.
func (c *LFUCache) GetIFPresent(key interface{}) (interface{}, error) {
	v, err := c.get(key, false)
	if err == ErrKeyNotFound {
		return c.getWithLoader(key, false)
	}
	return v, err
}

func (c *LFUCache) get(key interface{}, onLoad bool) (interface{}, error) {
	v, err := c.getValue(key, onLoad)
	if err != nil {
		return nil, err
	}
	if c.deserializeWith != nil {
		return c.deserializeWith(key, v)
	}
	return v, nil
}

func (c *LFUCache) getValue(key interface{}, onLoad bool) (interface{}, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	item, ok := c.items[key]
	if ok {
		if !item.IsExpired(nil) {
			c.increment(item)
			v := item.value
			c.mu.Unlock()
			if !onLoad {
				c.stats.IncrHitCount()
			}
			return v, nil
		}
		c.removeItem(item)
	}
	if !onLoad {
		c.stats.IncrMissCount()
	}
	return nil, ErrKeyNotFound
}

func (c *LFUCache) getWithLoader(key interface{}, isWait bool) (interface{}, error) {
	value, _, err := c.load(key, func(v interface{}, expiration *time.Duration, e error) (interface{}, error) {
		if e != nil {
			return nil, e
		}
		c.mu.Lock()
		defer c.mu.Unlock()
		item, err := c.set(key, v)
		if err != nil {
			return nil, err
		}
		if expiration != nil {
			t := c.clock.Now().Add(*expiration)
			item.(*lfuItem).expiration = &t
		}
		return v, nil
	}, isWait)
	if err != nil {
		return nil, err
	}
	return value, nil
}

func (c *LFUCache) increment(item *lfuItem) {
	currentFreqElement := item.freqElement
	currentFreqEntry := currentFreqElement.Value.(*freqEntry)
	nextFreq := currentFreqEntry.freq + 1
	delete(currentFreqEntry.items, item)

	nextFreqElement := currentFreqElement.Next()
	if nextFreqElement == nil {
		nextFreqElement = c.freqList.InsertAfter(&freqEntry{
			freq:  nextFreq,
			items: make(map[*lfuItem]struct{}),
		}, currentFreqElement)
	}
	nextFreqElement.Value.(*freqEntry).items[item] = struct{}{}
	item.freqElement = nextFreqElement
}

// evict removes the least frequent item from the cache.
func (c *LFUCache) evict(count int) {
	entry := c.freqList.Front()
	for i := 0; i < count; {
		if entry == nil {
			return
		}
		for item := range entry.Value.(*freqEntry).items {
			if i >= count {
				return
			}
			c.removeItem(item)
			if c.onEvict != nil {
				c.onEvict(item.key, item.value)
			}
			i++
		}
		entry = entry.Next()
	}
}

// Has checks if key exists in cache
func (c *LFUCache) Has(key interface{}) bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	now := time.Now()
	return c.has(key, &now)
}

func (c *LFUCache) has(key interface{}, now *time.Time) bool {
	item, ok := c.items[key]
	if !ok {
		return false
	}
	return !item.IsExpired(now)
}

// Remove removes the provided key from the cache.
func (c *LFUCache) Remove(key interface{}) bool {
	return c.remove(key)
}

func (c *LFUCache) remove(key interface{}) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if item, ok := c.items[key]; ok {
		c.removeItem(item)
		if c.onDel != nil {
			c.onDel(item.key, item.value)
		}
		return true
	}
	return false
}

// removeElement is used to remove a given list element from the cache
func (c *LFUCache) removeItem(item *lfuItem) {
	delete(c.items, item.key)
	delete(item.freqElement.Value.(*freqEntry).items, item)
}

func (c *LFUCache) keys() []interface{} {
	c.mu.RLock()
	defer c.mu.RUnlock()
	keys := make([]interface{}, len(c.items))
	var i = 0
	for k := range c.items {
		keys[i] = k
		i++
	}
	return keys
}

// GetALL returns all key-value pairs in the cache.
func (c *LFUCache) GetALL(checkExpired bool) map[interface{}]interface{} {
	c.mu.RLock()
	defer c.mu.RUnlock()
	items := make(map[interface{}]interface{}, len(c.items))
	now := time.Now()
	for k, item := range c.items {
		if !checkExpired || c.has(k, &now) {
			items[k] = item.value
		}
	}
	return items
}

// Keys returns a slice of the keys in the cache
func (c *LFUCache) Keys(checkExpired bool) []interface{} {
	c.mu.RLock()
	defer c.mu.RUnlock()
	keys := make([]interface{}, 0, len(c.items))
	now := time.Now()
	for k := range c.items {
		if !checkExpired || c.has(k, &now) {
			keys = append(keys, k)
		}
	}
	return keys
}

// Len returns the number of items in the cache
func (c *LFUCache) Len(checkExpired bool) int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if !checkExpired {
		return len(c.items)
	}
	var length int
	now := time.Now()
	for k := range c.items {
		if c.has(k, &now) {
			length++
		}
	}
	return length
}

// Purge completely clears the cache
func (c *LFUCache) Purge() {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.onPurge != nil {
		for key, item := range c.items {
			c.onPurge(key, item.value)
		}
	}

	c.init()
}

type freqEntry struct {
	freq  uint
	items map[*lfuItem]struct{}
}

type lfuItem struct {
	clock       Clock
	key         interface{}
	value       interface{}
	freqElement *list.Element
	expiration  *time.Time
}

// IsExpired returns boolean value whether this item is expired or not
func (it *lfuItem) IsExpired(now *time.Time) bool {
	if it.expiration == nil {
		return false
	}
	if now == nil {
		t := it.clock.Now()
		now = &t
	}
	return it.expiration.Before(*now)
}
