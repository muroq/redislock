package redislock_test

import (
	"context"
	"math/rand"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-redis/redis/v8"
	"github.com/muroq/redislock"
	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
)

const lockKey = "__bsm_redislock_unit_test__"

var _ = Describe("Client", func() {
	var subject *redislock.Client
	var ctx = context.Background()

	BeforeEach(func() {
		subject = redislock.New(redisClient)
	})

	AfterEach(func() {
		Expect(redisClient.Del(ctx, lockKey).Err()).To(Succeed())
	})

	It("should obtain once with TTL", func() {
		lock1, err := subject.Obtain(ctx, lockKey, time.Hour, time.Hour, nil)
		Expect(err).NotTo(HaveOccurred())
		Expect(lock1.Token()).To(HaveLen(22))
		Expect(lock1.TTL(ctx)).To(BeNumerically("~", time.Hour, time.Second))
		defer lock1.Release(ctx)

		_, err = subject.Obtain(ctx, lockKey, time.Hour, time.Hour, nil)
		Expect(err).To(Equal(redislock.ErrNotObtained))
		Expect(lock1.Release(ctx)).To(Succeed())

		lock2, err := subject.Obtain(ctx, lockKey, time.Minute, time.Minute, nil)
		Expect(err).NotTo(HaveOccurred())
		Expect(lock2.TTL(ctx)).To(BeNumerically("~", time.Minute, time.Second))
		Expect(lock2.Release(ctx)).To(Succeed())
	})

	It("should obtain through short-cut", func() {
		lock, err := redislock.Obtain(ctx, redisClient, lockKey, time.Hour, time.Hour, nil)
		Expect(err).NotTo(HaveOccurred())
		Expect(lock.Release(ctx)).To(Succeed())
	})

	It("should support custom metadata", func() {
		lock, err := redislock.Obtain(ctx, redisClient, lockKey, time.Hour, time.Hour, &redislock.Options{Metadata: "my-data"})
		Expect(err).NotTo(HaveOccurred())
		Expect(lock.Metadata()).To(Equal("my-data"))
		Expect(lock.Release(ctx)).To(Succeed())
	})

	It("should refresh", func() {
		lock, err := redislock.Obtain(ctx, redisClient, lockKey, time.Minute, time.Minute, nil)
		Expect(err).NotTo(HaveOccurred())
		Expect(lock.TTL(ctx)).To(BeNumerically("~", time.Minute, time.Second))
		Expect(lock.Refresh(ctx, time.Hour, nil)).To(Succeed())
		Expect(lock.TTL(ctx)).To(BeNumerically("~", time.Hour, time.Second))
		Expect(lock.Release(ctx)).To(Succeed())
	})

	It("should fail to release if expired", func() {
		lock, err := redislock.Obtain(ctx, redisClient, lockKey, time.Millisecond, time.Millisecond, nil)
		Expect(err).NotTo(HaveOccurred())
		time.Sleep(5 * time.Millisecond)
		Expect(lock.Release(ctx)).To(MatchError(redislock.ErrLockNotHeld))
	})

	It("should fail to release if ontained by someone else", func() {
		lock, err := redislock.Obtain(ctx, redisClient, lockKey, time.Minute, time.Minute, nil)
		Expect(err).NotTo(HaveOccurred())

		Expect(redisClient.Set(ctx, lockKey, "ABCD", 0).Err()).NotTo(HaveOccurred())
		Expect(lock.Release(ctx)).To(MatchError(redislock.ErrLockNotHeld))
	})

	It("should fail to refresh if expired", func() {
		lock, err := redislock.Obtain(ctx, redisClient, lockKey, time.Millisecond, time.Millisecond, nil)
		Expect(err).NotTo(HaveOccurred())
		time.Sleep(5 * time.Millisecond)
		Expect(lock.Refresh(ctx, time.Hour, nil)).To(MatchError(redislock.ErrNotObtained))
	})

	It("should retry if enabled", func() {
		// retry, succeed
		Expect(redisClient.Set(ctx, lockKey, "ABCD", 0).Err()).NotTo(HaveOccurred())
		Expect(redisClient.PExpire(ctx, lockKey, 20*time.Millisecond).Err()).NotTo(HaveOccurred())

		lock, err := redislock.Obtain(ctx, redisClient, lockKey, time.Hour, time.Hour, &redislock.Options{
			RetryStrategy: redislock.LimitRetry(redislock.LinearBackoff(100*time.Millisecond), 3),
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(lock.Release(ctx)).To(Succeed())

		// no retry, fail
		Expect(redisClient.Set(ctx, lockKey, "ABCD", 0).Err()).NotTo(HaveOccurred())
		Expect(redisClient.PExpire(ctx, lockKey, 50*time.Millisecond).Err()).NotTo(HaveOccurred())

		_, err = redislock.Obtain(ctx, redisClient, lockKey, time.Hour, time.Hour, nil)
		Expect(err).To(MatchError(redislock.ErrNotObtained))

		// retry 2x, give up & fail
		Expect(redisClient.Set(ctx, lockKey, "ABCD", 0).Err()).NotTo(HaveOccurred())
		Expect(redisClient.PExpire(ctx, lockKey, 50*time.Millisecond).Err()).NotTo(HaveOccurred())

		_, err = redislock.Obtain(ctx, redisClient, lockKey, time.Hour, time.Hour, &redislock.Options{
			RetryStrategy: redislock.LimitRetry(redislock.LinearBackoff(time.Millisecond), 2),
		})
		Expect(err).To(MatchError(redislock.ErrNotObtained))
	})

	It("should prevent multiple locks (fuzzing)", func() {
		numLocks := int32(0)
		wg := new(sync.WaitGroup)
		for i := 0; i < 1000; i++ {
			wg.Add(1)

			go func() {
				defer GinkgoRecover()
				defer wg.Done()

				wait := rand.Int63n(int64(50 * time.Millisecond))
				time.Sleep(time.Duration(wait))

				_, err := subject.Obtain(ctx, lockKey, time.Minute, time.Minute, nil)
				if err == redislock.ErrNotObtained {
					return
				}
				Expect(err).NotTo(HaveOccurred())
				atomic.AddInt32(&numLocks, 1)
			}()
		}
		wg.Wait()
		Expect(numLocks).To(Equal(int32(1)))
	})

})

var _ = Describe("RetryStrategy", func() {
	It("should support no-retry", func() {
		subject := redislock.NoRetry()
		Expect(subject.NextBackoff()).To(Equal(time.Duration(0)))
	})

	It("should support linear backoff", func() {
		subject := redislock.LinearBackoff(time.Second)
		Expect(subject.NextBackoff()).To(Equal(time.Second))
		Expect(subject.NextBackoff()).To(Equal(time.Second))
	})

	It("should support limits", func() {
		subject := redislock.LimitRetry(redislock.LinearBackoff(time.Second), 2)
		Expect(subject.NextBackoff()).To(Equal(time.Second))
		Expect(subject.NextBackoff()).To(Equal(time.Second))
		Expect(subject.NextBackoff()).To(Equal(time.Duration(0)))
	})

	It("should support exponential backoff", func() {
		subject := redislock.ExponentialBackoff(10*time.Millisecond, 300*time.Millisecond)
		Expect(subject.NextBackoff()).To(Equal(10 * time.Millisecond))
		Expect(subject.NextBackoff()).To(Equal(10 * time.Millisecond))
		Expect(subject.NextBackoff()).To(Equal(16 * time.Millisecond))
		Expect(subject.NextBackoff()).To(Equal(32 * time.Millisecond))
		Expect(subject.NextBackoff()).To(Equal(64 * time.Millisecond))
		Expect(subject.NextBackoff()).To(Equal(128 * time.Millisecond))
		Expect(subject.NextBackoff()).To(Equal(256 * time.Millisecond))
		Expect(subject.NextBackoff()).To(Equal(300 * time.Millisecond))
		Expect(subject.NextBackoff()).To(Equal(300 * time.Millisecond))
		Expect(subject.NextBackoff()).To(Equal(300 * time.Millisecond))
	})
})

// --------------------------------------------------------------------

func TestSuite(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "redislock")
}

var redisClient *redis.Client

var _ = BeforeSuite(func() {
	redisClient = redis.NewClient(&redis.Options{
		Network: "tcp",
		Addr:    "127.0.0.1:6379", DB: 9,
	})
	Expect(redisClient.Ping(context.Background()).Err()).To(Succeed())
})

var _ = AfterSuite(func() {
	Expect(redisClient.Close()).To(Succeed())
})
